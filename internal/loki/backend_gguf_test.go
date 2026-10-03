package loki

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ggufBuilder écrit un GGUF synthétique : en-tête, clés typées, table des
// tenseurs, sans aucune donnée de poids (le lecteur n'en lit jamais).
type ggufBuilder struct {
	bo      binary.ByteOrder
	version uint32
	nKV     int
	nT      int
	kv      bytes.Buffer
	tensors bytes.Buffer
}

func newGGUF() *ggufBuilder { return &ggufBuilder{bo: binary.LittleEndian, version: 3} }

func (b *ggufBuilder) cnt(w *bytes.Buffer, n uint64) {
	if b.version == 1 {
		binary.Write(w, b.bo, uint32(n))
	} else {
		binary.Write(w, b.bo, n)
	}
}

func (b *ggufBuilder) str(w *bytes.Buffer, s string) {
	b.cnt(w, uint64(len(s)))
	w.WriteString(s)
}

func (b *ggufBuilder) key(k string, t uint32) {
	b.nKV++
	b.str(&b.kv, k)
	binary.Write(&b.kv, b.bo, t)
}

func (b *ggufBuilder) u32(k string, v uint32) *ggufBuilder {
	b.key(k, ggufUint32)
	binary.Write(&b.kv, b.bo, v)
	return b
}

func (b *ggufBuilder) i64(k string, v int64) *ggufBuilder {
	b.key(k, ggufInt64)
	binary.Write(&b.kv, b.bo, v)
	return b
}

func (b *ggufBuilder) f32(k string, v float32) *ggufBuilder {
	b.key(k, ggufFloat32)
	binary.Write(&b.kv, b.bo, math.Float32bits(v))
	return b
}

func (b *ggufBuilder) boolean(k string, v bool) *ggufBuilder {
	b.key(k, ggufBool)
	if v {
		b.kv.WriteByte(1)
	} else {
		b.kv.WriteByte(0)
	}
	return b
}

func (b *ggufBuilder) s(k, v string) *ggufBuilder {
	b.key(k, ggufString)
	b.str(&b.kv, v)
	return b
}

func (b *ggufBuilder) strArr(k string, vs []string) *ggufBuilder {
	b.key(k, ggufArray)
	binary.Write(&b.kv, b.bo, ggufString)
	b.cnt(&b.kv, uint64(len(vs)))
	for _, v := range vs {
		b.str(&b.kv, v)
	}
	return b
}

func (b *ggufBuilder) i32Arr(k string, vs []int32) *ggufBuilder {
	b.key(k, ggufArray)
	binary.Write(&b.kv, b.bo, ggufInt32)
	b.cnt(&b.kv, uint64(len(vs)))
	for _, v := range vs {
		binary.Write(&b.kv, b.bo, v)
	}
	return b
}

// nested : tableau de deux tableaux de u8, pour vérifier le saut récursif.
func (b *ggufBuilder) nested(k string) *ggufBuilder {
	b.key(k, ggufArray)
	binary.Write(&b.kv, b.bo, ggufArray)
	b.cnt(&b.kv, 2)
	for i := 0; i < 2; i++ {
		binary.Write(&b.kv, b.bo, ggufUint8)
		b.cnt(&b.kv, 3)
		b.kv.Write([]byte{1, 2, 3})
	}
	return b
}

func (b *ggufBuilder) tensor(name string, dims ...uint64) *ggufBuilder {
	b.str(&b.tensors, name)
	binary.Write(&b.tensors, b.bo, uint32(len(dims)))
	for _, d := range dims {
		if b.version == 1 {
			binary.Write(&b.tensors, b.bo, uint32(d))
		} else {
			binary.Write(&b.tensors, b.bo, d)
		}
	}
	binary.Write(&b.tensors, b.bo, uint32(0))       // type F32
	binary.Write(&b.tensors, b.bo, uint64(b.nT*32)) // position des données
	b.nT++
	return b
}

func (b *ggufBuilder) bytes() []byte {
	var out bytes.Buffer
	out.WriteString("GGUF")
	binary.Write(&out, b.bo, b.version)
	b.cnt(&out, uint64(b.nT))
	b.cnt(&out, uint64(b.nKV))
	out.Write(b.kv.Bytes())
	out.Write(b.tensors.Bytes())
	// bourrage jusqu'à l'alignement par défaut, puis 32 octets de « poids » par
	// tenseur
	if b.nT > 0 {
		out.Write(make([]byte, (32-out.Len()%32)%32+32*b.nT))
	}
	return out.Bytes()
}

func writeGGUF(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// qwenMTP : modèle hybride à 4 couches, têtes KV par couche (0 sur les couches
// récurrentes), tête MTP sur la dernière couche, vocabulaire à sauter.
func qwenMTP(b *ggufBuilder) *ggufBuilder {
	return b.u32("general.alignment", 32).
		strArr("tokenizer.ggml.tokens", []string{"<s>", "a", "bb", strings.Repeat("c", 300)}).
		s("general.architecture", "qwen35").
		s("general.name", "Qwen synthétique").
		u32("qwen35.block_count", 4).
		u32("qwen35.nextn_predict_layers", 1).
		i64("qwen35.context_length", 262144).
		u32("qwen35.attention.key_length", 256).
		u32("qwen35.attention.value_length", 256).
		i32Arr("qwen35.attention.head_count_kv", []int32{0, 0, 0, 4}).
		u32("qwen35.full_attention_interval", 4).
		u32("qwen35.ssm.conv_kernel", 4).
		f32("qwen35.rope.freq_base", 1e7).
		boolean("tokenizer.ggml.add_bos_token", false).
		nested("bidon.imbrique").
		tensor("token_embd.weight", 1024, 32000).
		tensor("blk.0.attn_norm.weight", 1024).
		tensor("blk.3.nextn.eh_proj.weight", 2048, 1024)
}

func TestGGUFMetaQwenHybrideMTP(t *testing.T) {
	p := writeGGUF(t, t.TempDir(), "qwen.gguf", qwenMTP(newGGUF()).bytes())
	got, err := ggufMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	want := GGUFInfo{
		Version: 3, Arch: "qwen35", BlockCount: 4, NextN: 1, ContextLength: 262144,
		KeyLen: 256, ValLen: 256, HeadCountKV: 4, HeadCountKVLayers: []int{0, 0, 0, 4},
		FullAttnInterval: 4, Hybrid: true, SSMConv: 4, HasNextNTensor: true, TensorCount: 3,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ggufMeta =\n%+v\nattendu\n%+v", got, want)
	}
}

// La clé nextn_predict_layers ne prouve rien : seul le tenseur de la DERNIÈRE
// couche compte, comme dans llama.cpp.
func TestGGUFMetaTeteMTPAbsenteOuMalPlacee(t *testing.T) {
	dir := t.TempDir()
	base := func() *ggufBuilder {
		return newGGUF().s("general.architecture", "glm4moe").
			u32("glm4moe.block_count", 47).
			u32("glm4moe.nextn_predict_layers", 1).
			u32("glm4moe.expert_count", 128).
			u32("glm4moe.attention.head_count_kv", 8)
	}
	cases := []struct {
		name string
		b    *ggufBuilder
	}{
		{"sans-tenseur.gguf", base().tensor("blk.45.ffn_up.weight", 8, 8)},
		{"mauvaise-couche.gguf", base().tensor("blk.45.nextn.eh_proj.weight", 8, 8)},
	}
	for _, c := range cases {
		got, err := ggufMeta(writeGGUF(t, dir, c.name, c.b.bytes()))
		if err != nil {
			t.Fatalf("%s : %v", c.name, err)
		}
		if got.HasNextNTensor || got.NextN != 1 || got.ExpertCount != 128 || got.HeadCountKV != 8 ||
			got.HeadCountKVLayers != nil || got.Hybrid {
			t.Fatalf("%s : %+v", c.name, got)
		}
	}
	got, err := ggufMeta(writeGGUF(t, dir, "ok.gguf", base().tensor("blk.46.nextn.eh_proj.weight", 8, 8).bytes()))
	if err != nil || !got.HasNextNTensor {
		t.Fatalf("tête MTP en dernière couche non vue : %+v, %v", got, err)
	}
}

// Modèle en tranches : les clés sont dans la première, la tête MTP dans la
// dernière. N'importe quelle tranche peut être désignée.
func TestGGUFMetaTranches(t *testing.T) {
	dir := t.TempDir()
	first := newGGUF().s("general.architecture", "qwen35").u32("qwen35.block_count", 4).
		u32("split.count", 2).tensor("token_embd.weight", 8, 8)
	second := newGGUF().u32("split.no", 1).u32("split.count", 2).
		tensor("blk.3.nextn.eh_proj.weight", 8, 8).tensor("output.weight", 8, 8)
	p1 := writeGGUF(t, dir, "m-00001-of-00002.gguf", first.bytes())
	writeGGUF(t, dir, "m-00002-of-00002.gguf", second.bytes())
	for _, p := range []string{p1, filepath.Join(dir, "m-00002-of-00002.gguf")} {
		got, err := ggufMeta(p)
		if err != nil {
			t.Fatal(err)
		}
		if got.Arch != "qwen35" || !got.HasNextNTensor || got.TensorCount != 3 {
			t.Fatalf("%s : %+v", baseName(p), got)
		}
	}
	// tranche manquante : on ne sait pas, donc erreur
	writeGGUF(t, dir, "n-00001-of-00002.gguf", first.bytes())
	if _, err := ggufMeta(filepath.Join(dir, "n-00001-of-00002.gguf")); err == nil {
		t.Fatal("tranche manquante acceptée")
	}
	// gguf-split --no-tensor-first-split : la première tranche ne porte que les
	// clés, aucun tenseur ni aucune donnée. Elle doit passer telle quelle.
	meta := newGGUF().s("general.architecture", "glm4moe").u32("glm4moe.block_count", 47).
		u32("split.count", 2)
	rest := newGGUF().u32("split.no", 1).tensor("blk.46.nextn.eh_proj.weight", 8, 8)
	writeGGUF(t, dir, "s-00001-of-00002.gguf", meta.bytes())
	writeGGUF(t, dir, "s-00002-of-00002.gguf", rest.bytes())
	got, err := ggufMeta(filepath.Join(dir, "s-00001-of-00002.gguf"))
	if err != nil || got.Arch != "glm4moe" || !got.HasNextNTensor || got.TensorCount != 1 {
		t.Fatalf("première tranche sans tenseur : %+v, %v", got, err)
	}
}

// v1 (compteurs 32 bits) et gros-boutiste : même résultat que le v3 ordinaire.
func TestGGUFMetaVersionsEtOrdre(t *testing.T) {
	dir := t.TempDir()
	v1 := newGGUF()
	v1.version = 1
	v2 := newGGUF()
	v2.version = 2
	be := newGGUF()
	be.bo = binary.BigEndian
	for name, b := range map[string]*ggufBuilder{"v1.gguf": v1, "v2.gguf": v2, "be.gguf": be} {
		got, err := ggufMeta(writeGGUF(t, dir, name, qwenMTP(b).bytes()))
		if err != nil {
			t.Fatalf("%s : %v", name, err)
		}
		if got.Arch != "qwen35" || got.BlockCount != 4 || !got.HasNextNTensor ||
			!reflect.DeepEqual(got.HeadCountKVLayers, []int{0, 0, 0, 4}) || got.Version != int(b.version) {
			t.Fatalf("%s : %+v", name, got)
		}
	}
}

// Fichier en cours de téléchargement : chaque coupure donne une erreur, jamais
// un panic ni un faux résultat.
func TestGGUFMetaTronque(t *testing.T) {
	dir := t.TempDir()
	full := qwenMTP(newGGUF()).bytes()
	// Toutes les coupures jusqu'au premier octet du dernier tenseur (le seul que
	// le lecteur sait situer sans la table des types ggml).
	for cut := 0; cut <= len(full)-32; cut++ {
		if _, err := parseGGUF(bytes.NewReader(full[:cut]), int64(cut)); err == nil {
			t.Fatalf("coupure à %d/%d acceptée", cut, len(full))
		}
	}
	if _, err := parseGGUF(bytes.NewReader(full), int64(len(full))); err != nil {
		t.Fatalf("fichier complet refusé : %v", err)
	}
	// et par le chemin fichier, pour une coupure dans l'en-tête
	if _, err := ggufMeta(writeGGUF(t, dir, "cut.gguf", full[:len(full)/3])); err == nil {
		t.Fatal("fichier coupé accepté")
	}
}

func TestGGUFMetaFichiersAberrants(t *testing.T) {
	dir := t.TempDir()
	hdr := func(ver uint32, nT, nKV uint64) *bytes.Buffer {
		var w bytes.Buffer
		w.WriteString("GGUF")
		binary.Write(&w, binary.LittleEndian, ver)
		binary.Write(&w, binary.LittleEndian, nT)
		binary.Write(&w, binary.LittleEndian, nKV)
		return &w
	}
	// clé annoncée à 1 Eo
	hugeKey := hdr(3, 0, 1)
	binary.Write(hugeKey, binary.LittleEndian, uint64(1)<<60)
	// tableau de chaînes annoncé à 2^62 éléments
	hugeArr := hdr(3, 0, 1)
	binary.Write(hugeArr, binary.LittleEndian, uint64(1))
	hugeArr.WriteString("k")
	binary.Write(hugeArr, binary.LittleEndian, ggufArray)
	binary.Write(hugeArr, binary.LittleEndian, ggufString)
	binary.Write(hugeArr, binary.LittleEndian, uint64(1)<<62)
	// type de valeur inconnu
	badType := hdr(3, 0, 1)
	binary.Write(badType, binary.LittleEndian, uint64(1))
	badType.WriteString("k")
	binary.Write(badType, binary.LittleEndian, uint32(99))
	binary.Write(badType, binary.LittleEndian, uint64(0))
	// tenseur à 1000 dimensions
	badDims := hdr(3, 1, 0)
	binary.Write(badDims, binary.LittleEndian, uint64(1))
	badDims.WriteString("t")
	binary.Write(badDims, binary.LittleEndian, uint32(1000))
	badDims.Write(make([]byte, 64))

	cases := map[string][]byte{
		"vide":         nil,
		"alignement":   newGGUF().u32("general.alignment", 24).tensor("t", 1).bytes(),
		"signature":    []byte("GGML\x03\x00\x00\x00" + strings.Repeat("\x00", 16)),
		"version":      hdr(4, 0, 0).Bytes(),
		"compteurs":    hdr(3, 1<<40, 1<<40).Bytes(),
		"cle-geante":   hugeKey.Bytes(),
		"tableau":      hugeArr.Bytes(),
		"type-inconnu": badType.Bytes(),
		"dimensions":   badDims.Bytes(),
	}
	for name, data := range cases {
		if _, err := ggufMeta(writeGGUF(t, dir, name+".gguf", data)); err == nil {
			t.Errorf("%s : accepté", name)
		}
	}
	if _, err := ggufMeta(filepath.Join(dir, "absent.gguf")); err == nil {
		t.Error("fichier absent accepté")
	}
}

// Octet corrompu n'importe où dans l'en-tête (v1 et v3) : le lecteur doit rendre
// la main vite, sans panic remonté ni allocation démesurée. Les valeurs 0xFF et
// 0x7F transforment un compteur en nombre géant, le cas qui ferait mal.
func TestGGUFMetaOctetCorrompu(t *testing.T) {
	v1 := newGGUF()
	v1.version = 1
	for _, b := range []*ggufBuilder{newGGUF(), v1} {
		full := qwenMTP(b).bytes()
		hdr := len(full) - 3*32 // le bourrage et les « poids » ne sont jamais lus
		for i := 0; i < hdr; i++ {
			for _, v := range []byte{0xFF, 0x7F, 0x00} {
				mut := append([]byte(nil), full...)
				mut[i] = v
				// le filet de sécurité existe, mais aucune entrée ne doit y tomber
				if _, err := parseGGUF(bytes.NewReader(mut), int64(len(mut))); err != nil &&
					strings.Contains(err.Error(), "illisible") {
					t.Fatalf("v%d, octet %d = %#x : %v", b.version, i, v, err)
				}
			}
		}
	}
}

// Le cache tient tant que taille et date sont les mêmes, et se renouvelle dès
// qu'elles changent (téléchargement terminé, fichier remplacé).
func TestGGUFMetaCache(t *testing.T) {
	dir := t.TempDir()
	data := qwenMTP(newGGUF()).bytes()
	p := writeGGUF(t, dir, "c.gguf", data)
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(p, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	a, err := ggufMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	a.HeadCountKVLayers[0] = 99 // ne doit pas atteindre le cache

	// même taille, même date, contenu illisible : le cache répond
	writeGGUF(t, dir, "c.gguf", make([]byte, len(data)))
	if err := os.Chtimes(p, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	b, err := ggufMeta(p)
	if err != nil || b.HeadCountKVLayers[0] != 0 || !b.HasNextNTensor {
		t.Fatalf("cache non utilisé ou altéré : %+v, %v", b, err)
	}

	// date différente : relecture, donc erreur sur le contenu illisible
	if err := os.Chtimes(p, stamp.Add(time.Second), stamp.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := ggufMeta(p); err == nil {
		t.Fatal("fichier modifié servi depuis le cache")
	}
}
