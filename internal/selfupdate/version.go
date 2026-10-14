package selfupdate

import (
	"math/big"
	"regexp"
	"strings"
)

var versionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

func versionParts(v string) []string {
	p := versionPattern.FindStringSubmatch(v)
	if p == nil {
		return nil
	}
	for id := range strings.SplitSeq(p[4], ".") {
		if len(id) > 1 && id[0] == '0' && numeric(id) {
			return nil
		}
	}
	return p
}
func numeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func numberCompare(a, b string) int {
	x, _ := new(big.Int).SetString(a, 10)
	y, _ := new(big.Int).SetString(b, 10)
	return x.Cmp(y)
}

// Compare implements SemVer precedence including prereleases; metadata is ignored.
func Compare(a, b string) (int, bool) {
	x, y := versionParts(a), versionParts(b)
	if x == nil || y == nil {
		return 0, false
	}
	for i := 1; i <= 3; i++ {
		if c := numberCompare(x[i], y[i]); c != 0 {
			return c, true
		}
	}
	if x[4] == y[4] {
		return 0, true
	}
	if x[4] == "" {
		return 1, true
	}
	if y[4] == "" {
		return -1, true
	}
	xs, ys := strings.Split(x[4], "."), strings.Split(y[4], ".")
	for i := 0; i < len(xs) && i < len(ys); i++ {
		if xs[i] == ys[i] {
			continue
		}
		xn, yn := numeric(xs[i]), numeric(ys[i])
		if xn && yn {
			return numberCompare(xs[i], ys[i]), true
		}
		if xn {
			return -1, true
		}
		if yn {
			return 1, true
		}
		return strings.Compare(xs[i], ys[i]), true
	}
	if len(xs) < len(ys) {
		return -1, true
	}
	return 1, true
}
