package loki

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Lecteur des métadonnées GGUF. Plusieurs réglages du moteur dépendent de ce que
// le fichier contient VRAIMENT : un modèle qui embarque une tête MTP (prédiction
// de plusieurs jetons, vérifiée exactement par le modèle cible), le nombre de
// têtes KV par couche pour estimer le cache, une architecture hybride à état
// récurrent. Jusqu'ici Loki ne le savait qu'à travers le nom du fichier, ce qui
// ne prouve rien.
//
// On ne lit que l'en-tête, la section clé/valeur et la table des tenseurs : les
// poids eux-mêmes ne sont jamais touchés, quelques Mo au plus pour un modèle de
// 30 Go. Le fichier peut être en cours de téléchargement, tronqué, ou tout
// simplement pas un GGUF : chaque longueur lue est bornée par ce qui reste du
// fichier, et la moindre incohérence donne une erreur, jamais un plantage ni une
// allocation géante. Une erreur veut dire « on ne sait pas » : l'appelant reste
// sur le comportement prudent.

// GGUFInfo résume ce que Loki retient d'un modèle GGUF. Les champs absents du
// fichier restent à zéro.
type GGUFInfo struct {
	Version       int    // version du format (1 à 3)
	Arch          string // general.architecture
	BlockCount    int    // <arch>.block_count
	NextN         int    // <arch>.nextn_predict_layers (couches MTP annoncées)
	ContextLength int    // <arch>.context_length (contexte d'entraînement)
	ExpertCount   int    // <arch>.expert_count (0 = modèle dense)
	KeyLen        int    // <arch>.attention.key_length
	ValLen        int    // <arch>.attention.value_length
	// HeadCountKV : valeur unique, ou maximum sur les couches quand le fichier
	// donne un tableau par couche (HeadCountKVLayers, nil sinon). Sur un hybride,
	// les couches récurrentes y valent 0.
	HeadCountKV       int
	HeadCountKVLayers []int
	FullAttnInterval  int  // <arch>.full_attention_interval, si présent
	Hybrid            bool // au moins une clé <arch>.ssm.* : couches à état récurrent
	// Repli de llama.cpp quand key_length / value_length manquent : n_embd / n_head.
	EmbeddingLength int // <arch>.embedding_length
	HeadCount       int // <arch>.attention.head_count (valeur unique seulement)
	// Taille de l'état récurrent par couche (llama_hparams::n_embd_r / n_embd_s),
	// pour estimer ce que pèse un état sauvegardé d'un modèle hybride.
	SSMConv   int // <arch>.ssm.conv_kernel
	SSMInner  int // <arch>.ssm.inner_size
	SSMState  int // <arch>.ssm.state_size
	SSMGroups int // <arch>.ssm.group_count
	// HasNextNTensor : le tenseur blk.{BlockCount-1}.nextn.eh_proj.weight existe
	// dans l'une des tranches. C'est le test de llama.cpp lui-même pour reconnaître
	// une tête MTP : la clé nextn_predict_layers seule ne suffit pas, certains GGUF
	// la gardent alors que les tenseurs ont été retirés (tête publiée à part).
	HasNextNTensor bool
	TensorCount    int // total sur toutes les tranches
}

// Bornes de lecture. Elles sont larges devant les vrais modèles (une poignée de
// milliers de tenseurs, une cinquantaine de clés hors vocabulaire) et servent
// seulement à refuser un fichier aberrant avant d'allouer quoi que ce soit.
const (
	ggufMaxString     = 64 << 20 // modèle de chat le plus long connu : quelques dizaines de Ko
	ggufMaxKeyLen     = 64 << 10
	ggufMaxTensorName = 4096 // GGML_MAX_NAME vaut 64
	ggufMaxKV         = 1 << 20
	ggufMaxTensors    = 1 << 22
	ggufMaxDims       = 8 // GGML_MAX_DIMS vaut 4
	ggufMaxArrayDepth = 4
	ggufMaxLayerArray = 1 << 16
)

// Types de valeur GGUF (gguf.h).
const (
	ggufUint8 uint32 = iota
	ggufInt8
	ggufUint16
	ggufInt16
	ggufUint32
	ggufInt32
	ggufFloat32
	ggufBool
	ggufString
	ggufArray
	ggufUint64
	ggufInt64
	ggufFloat64
)

var errGGUFTruncated = errors.New("GGUF tronqué")

// ggufScalarSize : taille en octets d'un type de taille fixe, 0 sinon.
func ggufScalarSize(t uint32) int64 {
	switch t {
	case ggufUint8, ggufInt8, ggufBool:
		return 1
	case ggufUint16, ggufInt16:
		return 2
	case ggufUint32, ggufInt32, ggufFloat32:
		return 4
	case ggufUint64, ggufInt64, ggufFloat64:
		return 8
	}
	return 0
}

// ggufReader lit séquentiellement un GGUF en comptant ce qui reste du fichier :
// toute longueur annoncée au-delà est une incohérence, détectée AVANT de lire.
type ggufReader struct {
	r    *bufio.Reader
	bo   binary.ByteOrder
	v1   bool  // GGUF v1 : compteurs et longueurs sur 32 bits
	size int64 // taille totale du fichier
	left int64 // octets restants dans le fichier
	buf  [8]byte
}

func (g *ggufReader) fixed(n int) ([]byte, error) {
	if int64(n) > g.left {
		return nil, errGGUFTruncated
	}
	b := g.buf[:n]
	if _, err := io.ReadFull(g.r, b); err != nil {
		return nil, errGGUFTruncated
	}
	g.left -= int64(n)
	return b, nil
}

func (g *ggufReader) u32() (uint32, error) {
	b, err := g.fixed(4)
	if err != nil {
		return 0, err
	}
	return g.bo.Uint32(b), nil
}

func (g *ggufReader) u64() (uint64, error) {
	b, err := g.fixed(8)
	if err != nil {
		return 0, err
	}
	return g.bo.Uint64(b), nil
}

// count lit un compteur ou une longueur : 32 bits en v1, 64 bits ensuite.
func (g *ggufReader) count() (uint64, error) {
	if g.v1 {
		v, err := g.u32()
		return uint64(v), err
	}
	return g.u64()
}

// countSize : taille sur disque d'un compteur, pour borner les tableaux de chaînes.
func (g *ggufReader) countSize() int64 {
	if g.v1 {
		return 4
	}
	return 8
}

func (g *ggufReader) skip(n int64) error {
	if n < 0 || n > g.left {
		return errGGUFTruncated
	}
	for rest := n; rest > 0; {
		step := min(rest, 1<<30)
		if _, err := g.r.Discard(int(step)); err != nil {
			return errGGUFTruncated
		}
		rest -= step
	}
	g.left -= n
	return nil
}

// str lit une chaîne d'au plus max octets.
func (g *ggufReader) str(max uint64) (string, error) {
	n, err := g.count()
	if err != nil {
		return "", err
	}
	if n > max {
		return "", fmt.Errorf("chaîne GGUF de %d octets", n)
	}
	if int64(n) > g.left {
		return "", errGGUFTruncated
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(g.r, b); err != nil {
		return "", errGGUFTruncated
	}
	g.left -= int64(n)
	return string(b), nil
}

func (g *ggufReader) skipStr() error {
	n, err := g.count()
	if err != nil {
		return err
	}
	if n > ggufMaxString {
		return fmt.Errorf("chaîne GGUF de %d octets", n)
	}
	return g.skip(int64(n))
}

// intVal lit une valeur entière (ou booléenne) de type t. ok vaut false pour les
// flottants, lus et ignorés : aucune des clés retenues n'en est un.
func (g *ggufReader) intVal(t uint32) (v int64, ok bool, err error) {
	size := ggufScalarSize(t)
	if size == 0 {
		return 0, false, fmt.Errorf("type GGUF %d inattendu", t)
	}
	b, err := g.fixed(int(size))
	if err != nil {
		return 0, false, err
	}
	switch t {
	case ggufUint8, ggufBool:
		return int64(b[0]), true, nil
	case ggufInt8:
		return int64(int8(b[0])), true, nil
	case ggufUint16:
		return int64(g.bo.Uint16(b)), true, nil
	case ggufInt16:
		return int64(int16(g.bo.Uint16(b))), true, nil
	case ggufUint32:
		return int64(g.bo.Uint32(b)), true, nil
	case ggufInt32:
		return int64(int32(g.bo.Uint32(b))), true, nil
	case ggufUint64:
		u := g.bo.Uint64(b)
		if u > math.MaxInt64 {
			u = math.MaxInt64
		}
		return int64(u), true, nil
	case ggufInt64:
		return int64(g.bo.Uint64(b)), true, nil
	}
	return 0, false, nil // flottant
}

// skipArray saute n éléments de type et, tableaux imbriqués compris (profondeur
// bornée). Le vocabulaire (150 000 chaînes) passe par ici sans rien allouer.
func (g *ggufReader) skipArray(et uint32, n uint64, depth int) error {
	if depth > ggufMaxArrayDepth {
		return errors.New("tableaux GGUF trop imbriqués")
	}
	if size := ggufScalarSize(et); size > 0 {
		if n > uint64(g.left/size) {
			return errGGUFTruncated
		}
		return g.skip(int64(n) * size)
	}
	switch et {
	case ggufString:
		if n > uint64(g.left/g.countSize()) {
			return errGGUFTruncated
		}
		for i := uint64(0); i < n; i++ {
			if err := g.skipStr(); err != nil {
				return err
			}
		}
		return nil
	case ggufArray:
		// Chaque sous-tableau coûte au moins son type et son compteur.
		if n > uint64(g.left/(4+g.countSize())) {
			return errGGUFTruncated
		}
		for i := uint64(0); i < n; i++ {
			sub, err := g.u32()
			if err != nil {
				return err
			}
			sn, err := g.count()
			if err != nil {
				return err
			}
			if err := g.skipArray(sub, sn, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("type GGUF %d inconnu", et)
}

// ggufFile : ce qu'on retient d'UNE tranche avant de résoudre les clés selon
// l'architecture (general.architecture n'est pas forcément la première clé).
type ggufFile struct {
	version int
	arch    string
	ints    map[string]int64
	arrays  map[string][]int64 // tableaux d'entiers retenus (têtes KV par couche)
	keys    []string           // noms de toutes les clés, pour repérer <arch>.ssm.*
	tensors int
	nextn   []string // noms des tenseurs *.nextn.eh_proj.weight
}

// keepIntArray : les seuls tableaux qu'on lit au lieu de les sauter.
func keepIntArray(key string) bool {
	return strings.HasSuffix(key, ".attention.head_count_kv")
}

// readGGUFHeader lit l'en-tête, les clés et la table des tenseurs d'un fichier.
func readGGUFHeader(path string) (*ggufFile, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, fmt.Errorf("%s est un dossier", baseName(path))
	}
	f, err := parseGGUF(fh, st.Size())
	if err != nil {
		return nil, fmt.Errorf("%s : %w", baseName(path), err)
	}
	return f, nil
}

// parseGGUF lit size octets au plus de r. Un panic éventuel (fichier
// pathologique non prévu par les bornes) est rattrapé en erreur : lire des
// métadonnées ne doit jamais faire tomber Loki.
func parseGGUF(r io.Reader, size int64) (f *ggufFile, err error) {
	defer func() {
		if p := recover(); p != nil {
			f, err = nil, fmt.Errorf("GGUF illisible : %v", p)
		}
	}()
	g := &ggufReader{r: bufio.NewReaderSize(r, 1<<20), bo: binary.LittleEndian, size: size, left: size}
	magic, err := g.fixed(4)
	if err != nil {
		return nil, err
	}
	if string(magic) != "GGUF" {
		return nil, errors.New("pas un fichier GGUF")
	}
	ver, err := g.u32()
	if err != nil {
		return nil, err
	}
	// Les GGUF gros-boutistes (s390x) gardent la même signature : seule la
	// version lue à l'envers trahit l'ordre des octets.
	if ver&0xFFFF == 0 {
		if sw := bits.ReverseBytes32(ver); sw >= 1 && sw <= 3 {
			g.bo, ver = binary.BigEndian, sw
		}
	}
	if ver < 1 || ver > 3 {
		return nil, fmt.Errorf("version GGUF %d non prise en charge", ver)
	}
	g.v1 = ver == 1
	nTensors, err := g.count()
	if err != nil {
		return nil, err
	}
	nKV, err := g.count()
	if err != nil {
		return nil, err
	}
	if nKV > ggufMaxKV || nTensors > ggufMaxTensors {
		return nil, fmt.Errorf("compteurs GGUF aberrants (%d clés, %d tenseurs)", nKV, nTensors)
	}
	f = &ggufFile{version: int(ver), ints: map[string]int64{}, arrays: map[string][]int64{}, tensors: int(nTensors)}

	for i := uint64(0); i < nKV; i++ {
		key, err := g.str(ggufMaxKeyLen)
		if err != nil {
			return nil, err
		}
		t, err := g.u32()
		if err != nil {
			return nil, err
		}
		f.keys = append(f.keys, key)
		switch t {
		case ggufString:
			if key == "general.architecture" {
				if f.arch, err = g.str(ggufMaxKeyLen); err != nil {
					return nil, err
				}
			} else if err := g.skipStr(); err != nil {
				return nil, err
			}
		case ggufArray:
			et, err := g.u32()
			if err != nil {
				return nil, err
			}
			n, err := g.count()
			if err != nil {
				return nil, err
			}
			if keepIntArray(key) && et != ggufFloat32 && et != ggufFloat64 && ggufScalarSize(et) > 0 && n <= ggufMaxLayerArray {
				vals := make([]int64, 0, min(n, uint64(g.left)))
				for j := uint64(0); j < n; j++ {
					v, _, err := g.intVal(et)
					if err != nil {
						return nil, err
					}
					vals = append(vals, v)
				}
				f.arrays[key] = vals
			} else if err := g.skipArray(et, n, 1); err != nil {
				return nil, err
			}
		default:
			v, ok, err := g.intVal(t)
			if err != nil {
				return nil, err
			}
			if ok {
				f.ints[key] = v
			}
		}
	}

	dimSize := int64(8)
	if g.v1 {
		dimSize = 4
	}
	var maxOff uint64
	for i := uint64(0); i < nTensors; i++ {
		name, err := g.str(ggufMaxTensorName)
		if err != nil {
			return nil, err
		}
		nd, err := g.u32()
		if err != nil {
			return nil, err
		}
		if nd > ggufMaxDims {
			return nil, fmt.Errorf("tenseur %q à %d dimensions", name, nd)
		}
		// dimensions et type (u32), puis position des données (u64)
		if err := g.skip(int64(nd)*dimSize + 4); err != nil {
			return nil, err
		}
		off, err := g.u64()
		if err != nil {
			return nil, err
		}
		maxOff = max(maxOff, off)
		if strings.HasSuffix(name, ".nextn.eh_proj.weight") {
			f.nextn = append(f.nextn, name)
		}
	}

	// Les données commencent au prochain multiple de general.alignment. Le début
	// du dernier tenseur doit tenir dans le fichier : un GGUF coupé dans ses poids
	// (copie interrompue, disque plein) a un en-tête intact, il faut le voir
	// quand même. On ne connaît pas la taille de chaque tenseur sans la table des
	// types ggml : vérifier son premier octet suffit à attraper l'essentiel.
	align := int64(32)
	if a, ok := f.ints["general.alignment"]; ok {
		if a <= 0 || a > 1<<20 || a&(a-1) != 0 {
			return nil, fmt.Errorf("alignement GGUF %d invalide", a)
		}
		align = a
	}
	if nTensors > 0 {
		pos := g.size - g.left
		dataStart := (pos + align - 1) / align * align
		if maxOff >= uint64(g.size) || dataStart+int64(maxOff) >= g.size {
			return nil, errGGUFTruncated
		}
	}
	return f, nil
}

// clampInt ramène une valeur de métadonnée dans [0, MaxInt32] : un champ négatif
// ou démesuré n'a pas de sens pour un compte de couches ou de têtes.
func clampInt(v int64) int {
	if v < 0 {
		return 0
	}
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(v)
}

// ggufInfoFrom résout les clés de la première tranche selon l'architecture et
// cherche la tête MTP dans les tenseurs de toutes les tranches.
func ggufInfoFrom(first *ggufFile, all []*ggufFile) GGUFInfo {
	in := GGUFInfo{Version: first.version, Arch: first.arch}
	p := first.arch + "."
	get := func(k string) int { return clampInt(first.ints[p+k]) }
	in.BlockCount = get("block_count")
	in.NextN = get("nextn_predict_layers")
	in.ContextLength = get("context_length")
	in.ExpertCount = get("expert_count")
	in.KeyLen = get("attention.key_length")
	in.ValLen = get("attention.value_length")
	in.FullAttnInterval = get("full_attention_interval")
	in.EmbeddingLength = get("embedding_length")
	in.HeadCount = get("attention.head_count")
	in.SSMConv = get("ssm.conv_kernel")
	in.SSMInner = get("ssm.inner_size")
	in.SSMState = get("ssm.state_size")
	in.SSMGroups = get("ssm.group_count")
	in.HeadCountKV = get("attention.head_count_kv")
	if arr, ok := first.arrays[p+"attention.head_count_kv"]; ok {
		in.HeadCountKVLayers = make([]int, len(arr))
		in.HeadCountKV = 0
		for i, v := range arr {
			in.HeadCountKVLayers[i] = clampInt(v)
			in.HeadCountKV = max(in.HeadCountKV, in.HeadCountKVLayers[i])
		}
	}
	if first.arch != "" {
		for _, k := range first.keys {
			if strings.HasPrefix(k, p+"ssm.") {
				in.Hybrid = true
				break
			}
		}
	}
	want := ""
	if in.BlockCount > 0 {
		want = "blk." + strconv.Itoa(in.BlockCount-1) + ".nextn.eh_proj.weight"
	}
	for _, f := range all {
		in.TensorCount += f.tensors
		for _, n := range f.nextn {
			if n == want {
				in.HasNextNTensor = true
			}
		}
	}
	return in
}

// Cache par chemin : la même liste de modèles est consultée à chaque démarrage
// et à chaque affichage. L'empreinte (taille et date de chaque tranche) suffit à
// voir un fichier remplacé ou un téléchargement terminé ; les erreurs ne sont
// pas gardées, un fichier en cours de téléchargement sera relu une fois complet.
type ggufCacheEntry struct {
	stamp string
	info  GGUFInfo
}

var (
	ggufCacheMu sync.Mutex
	ggufCache   = map[string]ggufCacheEntry{}
)

const ggufCacheMax = 256

// ggufMeta lit les métadonnées du modèle désigné par path. Pour un modèle en
// tranches, path peut désigner n'importe laquelle : les clés viennent de la
// première (les suivantes ne portent que split.*), les tenseurs de toutes. Une
// tranche manquante est une erreur : le modèle ne démarrerait pas, et on ne
// saurait pas dire si la tête MTP s'y trouvait.
func ggufMeta(path string) (GGUFInfo, error) {
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	fam := shardFamily(path)
	paths := make([]string, len(fam))
	var stamp strings.Builder
	for i, n := range fam {
		paths[i] = filepath.Join(dir, n)
		st, err := os.Stat(paths[i])
		if err != nil {
			return GGUFInfo{}, err
		}
		if st.IsDir() {
			return GGUFInfo{}, fmt.Errorf("%s est un dossier", n)
		}
		fmt.Fprintf(&stamp, "%d:%d;", st.Size(), st.ModTime().UnixNano())
	}
	key := paths[0]

	ggufCacheMu.Lock()
	e, ok := ggufCache[key]
	ggufCacheMu.Unlock()
	if ok && e.stamp == stamp.String() {
		return cloneGGUFInfo(e.info), nil
	}

	files := make([]*ggufFile, 0, len(paths))
	for _, p := range paths {
		f, err := readGGUFHeader(p)
		if err != nil {
			return GGUFInfo{}, err
		}
		files = append(files, f)
	}
	info := ggufInfoFrom(files[0], files)

	ggufCacheMu.Lock()
	if len(ggufCache) >= ggufCacheMax {
		ggufCache = map[string]ggufCacheEntry{}
	}
	ggufCache[key] = ggufCacheEntry{stamp: stamp.String(), info: info}
	ggufCacheMu.Unlock()
	return cloneGGUFInfo(info), nil
}

// cloneGGUFInfo : l'appelant peut modifier le tableau par couche sans toucher au
// cache.
func cloneGGUFInfo(in GGUFInfo) GGUFInfo {
	if in.HeadCountKVLayers != nil {
		in.HeadCountKVLayers = append([]int(nil), in.HeadCountKVLayers...)
	}
	return in
}
