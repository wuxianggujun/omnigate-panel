package panelauth

import (
	"encoding/hex"
	"strings"
	"testing"
)

// TestPBKDF2SHA256Vectors 用公开测试向量钉住实现（RFC 8018 §5.2 的 SHA-256 版，
// 广泛引用的已知向量）。手写 PBKDF2 最怕"看起来能跑但算错"，这里逐一比对。
func TestPBKDF2SHA256Vectors(t *testing.T) {
	cases := []struct {
		pw, salt string
		iter     int
		dkLen    int
		want     string
	}{
		{"password", "salt", 1, 32, "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{"password", "salt", 2, 32, "ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{"password", "salt", 4096, 32, "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
		{"passwordPASSWORDpassword", "saltSALTsaltSALTsaltSALTsaltSALTsalt", 4096, 40,
			"348c89dbcbd32b2f32d814b8116e84cf2b17347ebc1800181c4e2a1fb8dd53e1c635518c7dac47e9"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(pbkdf2SHA256([]byte(c.pw), []byte(c.salt), c.iter, c.dkLen))
		if got != c.want {
			t.Errorf("pbkdf2(iter=%d,dkLen=%d)=%s want %s", c.iter, c.dkLen, got, c.want)
		}
	}
}

func TestHashVerifyRoundTrip(t *testing.T) {
	h, err := hashWithIters("correct horse", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$1000$") {
		t.Fatalf("hash format unexpected: %s", h)
	}
	if !VerifyPassword(h, "correct horse") {
		t.Error("correct password rejected")
	}
	if VerifyPassword(h, "wrong horse") {
		t.Error("wrong password accepted")
	}
}

// 同一密码两次哈希必须不同（盐随机），但都能校验通过。
func TestHashSaltIsRandom(t *testing.T) {
	a, _ := hashWithIters("pw", 1000)
	b, _ := hashWithIters("pw", 1000)
	if a == b {
		t.Error("two hashes of same password are identical (salt not random)")
	}
	if !VerifyPassword(a, "pw") || !VerifyPassword(b, "pw") {
		t.Error("hashed password failed to verify")
	}
}

func TestVerifyPasswordMalformed(t *testing.T) {
	for _, bad := range []string{
		"", "plaintext", "pbkdf2-sha256$0$c2FsdA$aGFzaA", "pbkdf2-sha256$100$!!!$aGFzaA",
		"bcrypt$100$c2FsdA$aGFzaA", "pbkdf2-sha256$100$c2FsdA$",
	} {
		if VerifyPassword(bad, "anything") {
			t.Errorf("malformed hash %q accepted", bad)
		}
	}
}
