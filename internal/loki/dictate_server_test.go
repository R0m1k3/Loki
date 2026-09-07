package loki

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// wavTest fabrique un WAV 16 kHz mono PCM 16 bits, comme celui que le
// navigateur envoie.
func wavTest(ech []int16, taux int) []byte {
	data := make([]byte, 2*len(ech))
	for i, v := range ech {
		binary.LittleEndian.PutUint16(data[2*i:], uint16(v))
	}
	b := make([]byte, 0, 44+len(data))
	ajout32 := func(v uint32) {
		var x [4]byte
		binary.LittleEndian.PutUint32(x[:], v)
		b = append(b, x[:]...)
	}
	ajout16 := func(v uint16) {
		var x [2]byte
		binary.LittleEndian.PutUint16(x[:], v)
		b = append(b, x[:]...)
	}
	b = append(b, "RIFF"...)
	ajout32(uint32(36 + len(data)))
	b = append(b, "WAVE"...)
	b = append(b, "fmt "...)
	ajout32(16)
	ajout16(1) // PCM entier
	ajout16(1) // mono
	ajout32(uint32(taux))
	ajout32(uint32(taux * 2))
	ajout16(2)  // alignement de bloc
	ajout16(16) // bits par échantillon
	b = append(b, "data"...)
	ajout32(uint32(len(data)))
	return append(b, data...)
}

func TestServerArgsFormeCleValeur(t *testing.T) {
	testHome(t)
	args, err := asrServerArgs(DictateCfg{Model: "parakeet-tdt-0.6b-v3"}, 4242)
	if err != nil {
		t.Fatal(err)
	}
	joint := strings.Join(args, " ")
	// sherpa-onnx ne reconnaît QUE --clef=valeur. La forme séparée par une
	// espace le laisse démarrer sans modèle, sans se plaindre.
	for _, a := range args {
		if !strings.HasPrefix(a, "--") || !strings.Contains(a, "=") {
			t.Errorf("argument %q : sherpa-onnx exige la forme --clef=valeur", a)
		}
	}
	for _, attendu := range []string{"--model-type=nemo_transducer", "--host=127.0.0.1", "--port=4242"} {
		if !strings.Contains(joint, attendu) {
			t.Errorf("argument manquant : %s\nobtenu : %s", attendu, joint)
		}
	}
	// Les quatre fichiers du modèle, sinon le serveur démarre puis meurt.
	for _, f := range asrFichiers {
		if !strings.Contains(joint, f) {
			t.Errorf("le fichier %s du modèle n'est pas passé au serveur", f)
		}
	}
}

func TestServerArgsRefuseModeleInconnu(t *testing.T) {
	testHome(t)
	if _, err := asrServerArgs(DictateCfg{Model: "nawak"}, 1234); err == nil {
		t.Error("un modèle hors catalogue doit être refusé avant le lancement")
	}
}

// Le serveur ne doit jamais prendre une carte : le binaire livré est le build
// CPU, et la VRAM appartient au moteur de chat.
func TestServerEnvMasqueLesGPU(t *testing.T) {
	got := asrServerEnv([]string{"PATH=/bin", "CUDA_VISIBLE_DEVICES=0,1", "HOME=/root"})
	n := 0
	for _, v := range got {
		if strings.HasPrefix(v, "CUDA_VISIBLE_DEVICES=") {
			n++
			if v != "CUDA_VISIBLE_DEVICES=" {
				t.Errorf("CUDA_VISIBLE_DEVICES = %q, attendu vide", v)
			}
		}
	}
	// Une variable en double laisse le gagnant dépendre de la libc.
	if n != 1 {
		t.Errorf("%d occurrences de CUDA_VISIBLE_DEVICES, attendu exactement 1", n)
	}
	if !contientTout(got, "PATH=/bin", "HOME=/root") {
		t.Error("le reste de l'environnement doit être conservé")
	}
}

func contientTout(l []string, vals ...string) bool {
	for _, v := range vals {
		trouve := false
		for _, x := range l {
			if x == v {
				trouve = true
				break
			}
		}
		if !trouve {
			return false
		}
	}
	return true
}

func TestPortLibre(t *testing.T) {
	p, err := portLibre()
	if err != nil {
		t.Fatal(err)
	}
	if p < 1024 || p > 65535 {
		t.Errorf("port = %d, hors de la plage utilisable", p)
	}
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
	if err != nil {
		t.Fatalf("port %d annoncé libre mais inutilisable : %v", p, err)
	}
	_ = l.Close()
}

// ─── Conversion WAV → float32 ────────────────────────────────────────────────

// L'offset 44 codé en dur est le piège classique : un WAV n'a pas d'en-tête de
// taille garantie, et des blocs (LIST, fact…) s'intercalent avant `data`. Les
// prendre pour de l'audio produit un craquement au début de chaque phrase.
func TestWavIgnoreLesBlocsIntercales(t *testing.T) {
	base := wavTest([]int16{0, 16384, -16384, 32767}, 16000)
	// Insère un bloc « LIST » de 6 octets juste après l'en-tête RIFF/WAVE.
	extra := append([]byte("LIST"), 6, 0, 0, 0, 'I', 'N', 'F', 'O', 'x', 'y')
	avec := append(append(append([]byte{}, base[:12]...), extra...), base[12:]...)
	binary.LittleEndian.PutUint32(avec[4:8], uint32(len(avec)-8))

	taux, ech, err := wavVersFloat32(avec)
	if err != nil {
		t.Fatalf("wavVersFloat32 : %v", err)
	}
	if taux != 16000 {
		t.Errorf("taux = %d, attendu 16000", taux)
	}
	if len(ech) != 4 {
		t.Fatalf("%d échantillons, attendu 4 — un bloc intercalé a été pris pour de l'audio", len(ech))
	}
	if ech[0] != 0 || math.Abs(float64(ech[1])-0.5) > 0.001 {
		t.Errorf("échantillons mal normalisés : %v", ech)
	}
}

func TestWavRefuseCeQuiNEnEstPas(t *testing.T) {
	for nom, in := range map[string][]byte{
		"vide":       {},
		"pas un WAV": []byte("ceci n'est pas un fichier audio du tout"),
		"sans data":  append([]byte("RIFF\x00\x00\x00\x00WAVE"), "fmt "...),
	} {
		if _, _, err := wavVersFloat32(in); err == nil {
			t.Errorf("%s : accepté alors qu'il est illisible", nom)
		}
	}
}

// Le stéréo doit être MOYENNÉ, pas réduit au premier canal : un navigateur qui
// livrerait deux canaux donnerait sinon un silence une fois sur deux.
func TestPcmStereoMoyenne(t *testing.T) {
	data := make([]byte, 8) // deux trames stéréo
	binary.LittleEndian.PutUint16(data[0:], uint16(int16(0)))
	binary.LittleEndian.PutUint16(data[2:], uint16(int16(32767)))
	binary.LittleEndian.PutUint16(data[4:], uint16(0x8000)) // -32768
	binary.LittleEndian.PutUint16(data[6:], uint16(int16(32767)))
	ech, err := pcmVersFloat32(data, 1, 16, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ech) != 2 {
		t.Fatalf("%d échantillons, attendu 2", len(ech))
	}
	if math.Abs(float64(ech[0])-0.5) > 0.01 {
		t.Errorf("premier échantillon = %v, attendu la moyenne des deux canaux (~0,5)", ech[0])
	}
	if math.Abs(float64(ech[1])) > 0.01 {
		t.Errorf("second échantillon = %v, attendu ~0 (canaux opposés)", ech[1])
	}
}

func TestPcmFormatNonGere(t *testing.T) {
	if _, err := pcmVersFloat32(make([]byte, 8), 1, 24, 1); err == nil {
		t.Error("un PCM 24 bits doit être refusé explicitement, pas décodé de travers")
	}
}

// ─── Protocole WebSocket ─────────────────────────────────────────────────────

// serveurASRFactice rejoue le protocole de sherpa-onnx : il lit l'en-tête et
// les échantillons, puis répond en JSON. Il vérifie au passage que le client
// annonce le BON nombre d'octets — un compte faux et le vrai serveur attend
// indéfiniment de quoi finir, sans jamais répondre.
func serveurASRFactice(t *testing.T, texte string) (adresse string, arret func(), recu *struct {
	Taux int
	N    int
}) {
	t.Helper()
	recu = &struct {
		Taux int
		N    int
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		// Le vrai serveur reconstitue l'utterance a partir du compte annonce :
		// il ne borne pas la taille des trames recues, ce test non plus.
		c.SetReadLimit(-1)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var buf []byte
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			buf = append(buf, b...)
			if len(buf) < asrEnteteOctets {
				continue
			}
			recu.Taux = int(binary.LittleEndian.Uint32(buf[0:4]))
			attendu := int(binary.LittleEndian.Uint32(buf[4:8]))
			if len(buf)-asrEnteteOctets < attendu {
				continue
			}
			recu.N = attendu / 4
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"text":"`+texte+`"}`))
			return
		}
	}))
	return "ws" + strings.TrimPrefix(srv.URL, "http"), srv.Close, recu
}

func TestInferEnvoieLeBonEnTete(t *testing.T) {
	adresse, arret, recu := serveurASRFactice(t, "  bonjour ceci est un test  ")
	defer arret()

	wav := wavTest(make([]int16, 800), 16000)
	txt, err := asrInferSur(context.Background(), adresse, wav)
	if err != nil {
		t.Fatalf("asrInferSur : %v", err)
	}
	if txt != "bonjour ceci est un test" {
		t.Errorf("texte = %q, les espaces de bord doivent être retirés", txt)
	}
	if recu.Taux != 16000 {
		t.Errorf("taux annoncé = %d, attendu 16000", recu.Taux)
	}
	if recu.N != 800 {
		t.Errorf("%d échantillons annoncés, attendu 800 — un compte faux fige le serveur", recu.N)
	}
}

// Un audio plus gros qu'une trame doit arriver ENTIER : le serveur recolle les
// morceaux à partir du compte annoncé, encore faut-il tout envoyer.
func TestInferDecoupeLesGrosEnvois(t *testing.T) {
	adresse, arret, recu := serveurASRFactice(t, "ok")
	defer arret()

	n := asrTrameMax // > une trame une fois converti en float32
	if _, err := asrInferSur(context.Background(), adresse, wavTest(make([]int16, n), 16000)); err != nil {
		t.Fatalf("asrInferSur : %v", err)
	}
	if recu.N != n {
		t.Errorf("%d échantillons reçus, attendu %d", recu.N, n)
	}
}

// Sur du silence, le moteur peut rendre une annotation plutôt qu'un texte. La
// laisser passer la collerait telle quelle dans le champ de saisie.
func TestInferFiltreLesMarqueurs(t *testing.T) {
	for _, marqueur := range []string{"[BLANK_AUDIO]", "(silence)", "[SOUND]", " [ Silence ] ", "  "} {
		adresse, arret, _ := serveurASRFactice(t, marqueur)
		txt, err := asrInferSur(context.Background(), adresse, wavTest(make([]int16, 100), 16000))
		arret()
		if err != nil {
			t.Fatalf("asrInferSur(%q) : %v", marqueur, err)
		}
		if txt != "" {
			t.Errorf("marqueur %q rendu comme texte %q, attendu vide", marqueur, txt)
		}
	}
}

func TestInferServeurInjoignable(t *testing.T) {
	p, err := portLibre()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = asrInferSur(ctx, "ws://127.0.0.1:"+strconv.Itoa(p), wavTest(make([]int16, 100), 16000))
	if err == nil {
		t.Error("un serveur injoignable doit remonter une erreur")
	}
}

func TestInferReponseIllisible(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		// Le vrai serveur reconstitue l'utterance a partir du compte annonce :
		// il ne borne pas la taille des trames recues, ce test non plus.
		c.SetReadLimit(-1)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
		_ = c.Write(ctx, websocket.MessageText, []byte("ceci n'est pas du JSON"))
	}))
	defer srv.Close()
	_, err := asrInferSur(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), wavTest(make([]int16, 100), 16000))
	if err == nil {
		t.Error("une réponse illisible doit remonter une erreur")
	}
}

// Un audio vide n'est pas une panne : on rend une transcription vide sans même
// ouvrir de connexion.
func TestInferAudioVide(t *testing.T) {
	txt, err := asrInferSur(context.Background(), "ws://127.0.0.1:1", wavTest(nil, 16000))
	if err != nil {
		t.Fatalf("un WAV sans échantillon ne doit pas être une erreur : %v", err)
	}
	if txt != "" {
		t.Errorf("texte = %q, attendu vide", txt)
	}
}

// ─── Supervision ─────────────────────────────────────────────────────────────

// Un modèle absent doit être annoncé comme tel, pas produire un lancement du
// serveur voué à mourir sur un fichier introuvable.
func TestEnsureModeleAbsent(t *testing.T) {
	testHome(t)
	if err := dictateCfgSave(DictateCfg{Model: "parakeet-tdt-0.6b-v3", Reactivity: "moyen"}); err != nil {
		t.Fatal(err)
	}
	_, err := asrEnsure()
	if err == nil {
		t.Fatal("un modèle absent doit produire une erreur")
	}
	if !strings.Contains(err.Error(), "absent") {
		t.Errorf("erreur = %q, elle doit dire que le modèle est absent", err)
	}
}

func TestEtatSansServeur(t *testing.T) {
	testHome(t)
	e := asrEtat()
	if e["actif"] != false {
		t.Errorf("actif = %v, attendu false sans serveur lancé", e["actif"])
	}
	for _, clef := range []string{"modele", "present", "device"} {
		if _, ok := e[clef]; !ok {
			t.Errorf("clé %q manquante — l'UI en a besoin", clef)
		}
	}
	// Le binaire livré est CPU : l'état ne doit pas laisser croire à une carte.
	if e["device"] != "cpu" {
		t.Errorf("device = %v, attendu \"cpu\"", e["device"])
	}
}

func TestEtatSerialisable(t *testing.T) {
	testHome(t)
	if _, err := json.Marshal(asrEtat()); err != nil {
		t.Fatalf("l'état doit être sérialisable pour /api/dictate/state : %v", err)
	}
}
