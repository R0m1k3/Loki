package loki

import (
	"fmt"
	"reflect"
	"testing"
	"testing/fstest"
)

// cpuFS fabrique un /proc + /sys minimal : online, l'affinité du processus et,
// pour chaque CPU en ligne, sa chaîne thread_siblings (siblings[cpu]).
func cpuFS(online, allowed string, siblings map[int]string, extra map[string]string) fstest.MapFS {
	m := fstest.MapFS{
		"sys/devices/system/cpu/online": {Data: []byte(online + "\n")},
		"proc/self/status":              {Data: []byte("Name:\tllama-server\nCpus_allowed:\tffff\nCpus_allowed_list:\t" + allowed + "\nMems_allowed:\t1\n")},
	}
	for c, s := range siblings {
		m[fmt.Sprintf("sys/devices/system/cpu/cpu%d/topology/thread_siblings", c)] = &fstest.MapFile{Data: []byte(s + "\n")}
	}
	for k, v := range extra {
		m[k] = &fstest.MapFile{Data: []byte(v)}
	}
	return m
}

// smtPairs : n cœurs à deux threads, frères adjacents (0-1, 2-3…) — la
// numérotation que suppose cpu_count_math_cpus.
func smtPairs(cores, from int) map[int]string {
	m := map[int]string{}
	for i := 0; i < cores; i++ {
		s := fmt.Sprintf("core%d", from/2+i)
		m[from+2*i], m[from+2*i+1] = s, s
	}
	return m
}

// hybrid : 8 cœurs P à deux threads (CPU 0-15) + 8 cœurs E (CPU 16-23).
func hybrid() map[int]string {
	m := smtPairs(8, 0)
	for c := 16; c < 24; c++ {
		m[c] = fmt.Sprintf("ecore%d", c)
	}
	return m
}

func TestCPUSetThreads(t *testing.T) {
	cases := []struct {
		name string
		fs   fstest.MapFS
		n    int
	}{
		{"machine entière, sans quota : rien", cpuFS("0-15", "0-15", smtPairs(8, 0), nil), 0},
		{"cpuset de 8 threads SMT : 4 cœurs", cpuFS("0-15", "0-7", smtPairs(8, 0), nil), 4},
		{
			// L'exemple qui tournait mal : tout sauf cpu0 sur un Intel hybride.
			// Les cœurs E sont écartés, cpu1 partage son cœur avec cpu0 → 8, pas 15.
			"hybride, tout sauf cpu0 : 8 cœurs P",
			cpuFS("0-23", "1-23", hybrid(), map[string]string{"sys/devices/cpu_atom/cpus": "16-23\n"}), 8,
		},
		{
			"hybride, cantonné aux cœurs E : on les prend",
			cpuFS("0-23", "16-19", hybrid(), map[string]string{"sys/devices/cpu_atom/cpus": "16-23\n"}), 4,
		},
		{
			"quota v2 de 2,5 CPU : arrondi à 3",
			cpuFS("0-15", "0-15", smtPairs(8, 0), map[string]string{"sys/fs/cgroup/cpu.max": "250000 100000\n"}), 3,
		},
		{
			"quota v2 illimité : rien",
			cpuFS("0-15", "0-15", smtPairs(8, 0), map[string]string{"sys/fs/cgroup/cpu.max": "max 100000\n"}), 0,
		},
		{
			"quota plus large que les cœurs : rien",
			cpuFS("0-15", "0-15", smtPairs(8, 0), map[string]string{"sys/fs/cgroup/cpu.max": "1200000 100000\n"}), 0,
		},
		{
			"quota v1 de 2 CPU",
			cpuFS("0-15", "0-15", smtPairs(8, 0), map[string]string{
				"sys/fs/cgroup/cpu,cpuacct/cpu.cfs_quota_us":  "200000\n",
				"sys/fs/cgroup/cpu,cpuacct/cpu.cfs_period_us": "100000\n"}), 2,
		},
		{
			"quota v1 illimité (-1) : rien",
			cpuFS("0-15", "0-15", smtPairs(8, 0), map[string]string{
				"sys/fs/cgroup/cpu/cpu.cfs_quota_us":  "-1\n",
				"sys/fs/cgroup/cpu/cpu.cfs_period_us": "100000\n"}), 0,
		},
		{
			"cpuset ET quota : le plus petit",
			cpuFS("0-15", "0-7", smtPairs(8, 0), map[string]string{"sys/fs/cgroup/cpu.max": "200000 100000"}), 2,
		},
		{
			"topologie illisible : on ne devine pas",
			cpuFS("0-15", "0-7", map[int]string{0: "core0"}, nil), 0,
		},
		{"pas de /proc : rien", fstest.MapFS{"sys/devices/system/cpu/online": {Data: []byte("0-3")}}, 0},
		{"liste illisible : rien", cpuFS("0-15", "0-x", smtPairs(8, 0), nil), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cpusetThreads(c.fs)
			if got.N != c.n {
				t.Fatalf("N = %d (%+v), want %d", got.N, got, c.n)
			}
			if got.N > 0 && (got.Why == "" || got.Engine <= got.N) {
				t.Fatalf("budget incomplet : %+v", got)
			}
		})
	}
}

func TestParseCPUList(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []int
		ok   bool
	}{
		{"0-3,8-9\n", []int{0, 1, 2, 3, 8, 9}, true},
		{" 5 ", []int{5}, true},
		{"2,0-2", []int{0, 1, 2}, true},
		{"", []int{}, true},
		{"3-1", nil, false},
		{"a", nil, false},
	} {
		got, err := parseCPUList(c.in)
		if (err == nil) != c.ok || (c.ok && !reflect.DeepEqual(got, c.want)) {
			t.Errorf("parseCPUList(%q) = %v, %v ; want %v", c.in, got, err, c.want)
		}
	}
}

func TestThreadCount(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		note bool
	}{
		{"", 0, false}, {"0", 0, false}, {" 8 ", 8, false}, {"auto", 0, true}, {"-1", 0, true},
	} {
		n, note := threadCount("THREADS", c.in)
		if n != c.n || (note != "") != c.note {
			t.Errorf("threadCount(%q) = %d, %q", c.in, n, note)
		}
	}
}

func TestFlagValue(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, ""},
		{[]string{"-t", "6"}, "6"},
		{[]string{"--threads=7"}, "7"},
		{[]string{"-t", "6", "--threads", "8"}, "8"}, // la dernière gagne, comme dans le moteur
		{[]string{"-tb", "9"}, ""},                   // -tb n'est pas -t
		{[]string{"-t"}, ""},                         // drapeau orphelin
	} {
		if got := flagValue(c.args, "-t", "--threads"); got != c.want {
			t.Errorf("flagValue(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}
