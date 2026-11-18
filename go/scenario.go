// Vendored scenario derivation (from the kit's wire.ts, same logic as the
// Node scenario.ts): the suite's scenario is generated from the FIXED seed
// 0x6a09e667f3bcc909, so the token owner's login is deterministic and
// identical on every trial. Viewer = users[0] of generateUsers(
// scenarioRng('users'), 12). Expected: adaiversen, id 1000.

package main

const scenarioSeed uint64 = 0x6a09e667f3bcc909

func fnv1a64(text string) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		hash ^= uint64(text[i])
		hash *= 0x100000001b3
	}
	return hash
}

func mix64(value uint64) uint64 {
	z := value + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// Rng is the kit's xorshift64* generator.
type Rng struct {
	state uint64
}

func newRng(seed uint64) *Rng {
	if seed == 0 {
		seed = scenarioSeed
	}
	return &Rng{state: seed}
}

func (r *Rng) nextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545f4914f6cdd1d
}

func (r *Rng) nextU32() uint32 {
	return uint32(r.nextU64() >> 32)
}

func (r *Rng) nextInt(maxExclusive int) int {
	if maxExclusive == 1 {
		return 0
	}
	const rngRange uint64 = 4294967296
	limit := uint32(rngRange - (rngRange % uint64(maxExclusive)))
	for {
		v := r.nextU32()
		if v < limit {
			return int(v % uint32(maxExclusive))
		}
	}
}

func (r *Rng) pick(items []string) string {
	return items[r.nextInt(len(items))]
}

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}

var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

// ScenarioUser is one generated user; users[0] is the viewer.
type ScenarioUser struct {
	ID    int
	Login string
	Name  string
	Email string
}

func scenarioViewer() ScenarioUser {
	rng := newRng(mix64(scenarioSeed ^ fnv1a64("users")))
	used := make(map[string]bool)
	var viewer ScenarioUser
	for i := 0; i < 12; i++ {
		first := rng.pick(firstNames)
		last := rng.pick(lastNames)
		base := ""
		for _, c := range first + last {
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
				base += string(c)
			}
		}
		login := base
		suffix := 2
		for used[login] {
			login = base + "-" + itoa(suffix)
			suffix++
		}
		used[login] = true
		user := ScenarioUser{ID: 1000 + i, Login: login, Name: first + " " + last, Email: login + "@example.invalid"}
		if i == 0 {
			viewer = user
		}
	}
	return viewer
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
