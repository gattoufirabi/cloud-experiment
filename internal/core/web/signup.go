package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- proof of work ----------

type PowStore struct {
	Key  []byte
	mu   sync.Mutex
	Used map[string]time.Time
}

func (p *PowStore) sign(s string) string {
	m := hmac.New(sha256.New, p.Key)
	m.Write([]byte(s))
	return hex.EncodeToString(m.Sum(nil))
}

// challenge: "<expires>.<salt>.<bits>.<hmac>", valid 10 minutes.
func (p *PowStore) Challenge(bitsNeeded int) string {
	salt := make([]byte, 12)
	_, _ = rand.Read(salt)
	body := fmt.Sprintf("%d.%s.%d", time.Now().Add(10*time.Minute).Unix(), hex.EncodeToString(salt), bitsNeeded)
	return body + "." + p.sign(body)
}

func leadingZeroBits(b []byte) int {
	n := 0
	for _, x := range b {
		if x == 0 {
			n += 8
			continue
		}
		return n + bits.LeadingZeros8(x)
	}
	return n
}

// verify checks the signature, expiry, difficulty and the work, and burns the challenge.
func (p *PowStore) Verify(ch, nonce string, minBits int) bool {
	parts := strings.Split(ch, ".")
	if len(parts) != 4 || len(nonce) == 0 || len(nonce) > 32 {
		return false
	}
	body := strings.Join(parts[:3], ".")
	if !hmac.Equal([]byte(p.sign(body)), []byte(parts[3])) {
		return false
	}
	exp, err1 := strconv.ParseInt(parts[0], 10, 64)
	need, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || time.Now().Unix() > exp || need < minBits {
		return false
	}
	sum := sha256.Sum256([]byte(ch + ":" + nonce))
	if leadingZeroBits(sum[:]) < need {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for k, t := range p.Used {
		if now.After(t) {
			delete(p.Used, k)
		}
	}
	if _, seen := p.Used[ch]; seen {
		return false
	}
	p.Used[ch] = time.Unix(exp, 0)
	return true
}

func Contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
