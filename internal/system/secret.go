package system

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

// Alphabets chosen so generated values survive being pasted into a shell, a
// wp-config.php string literal and a SQL statement without escaping.
const (
	passwordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	nameAlphabet     = "abcdefghijkmnopqrstuvwxyz0123456789"
)

// Password returns a cryptographically random password.
//
// Sampling is unbiased. The previous implementation reduced a random byte
// modulo a 62-character alphabet, which makes the first 8 characters of that
// alphabet meaningfully more likely than the rest and quietly costs entropy.
func Password(length int) (string, error) {
	if length < 12 {
		length = 12
	}
	return randomString(length, passwordAlphabet)
}

// Suffix returns a short random string for disambiguating generated names.
func Suffix(length int) (string, error) {
	return randomString(length, nameAlphabet)
}

func randomString(n int, alphabet string) (string, error) {
	out := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		// crypto/rand.Int rejects out-of-range draws internally, so the
		// distribution over the alphabet is uniform.
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("generating random value: %w", err)
		}
		out[i] = alphabet[v.Int64()]
	}
	return string(out), nil
}
