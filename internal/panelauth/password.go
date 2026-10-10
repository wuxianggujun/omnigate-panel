// password.go 面板账号密码的哈希与校验（PBKDF2-HMAC-SHA256，标准库实现）。
//
// 为什么手写而不引第三方：项目刻意维持零外部依赖（go.mod 只有 redis 及间接项），
// 而 golang.org/x/crypto/bcrypt / pbkdf2 会新增直接依赖；Dockerfile 用的是
// golang:1.23 构建镜像，标准库的 crypto/pbkdf2 要 Go 1.24 才有，用不了。
// PBKDF2-HMAC-SHA256 是 RFC 8018 定义的确定性算法，标准库 crypto/hmac +
// crypto/sha256 足够实现，且可用已知向量钉住正确性（见 password_test.go）。
package panelauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// 哈希参数。迭代次数取 OWASP 对 PBKDF2-HMAC-SHA256 的推荐下限量级；
// 校验时以存储里记录的迭代次数为准（历史哈希升级迭代数后仍可校验）。
const (
	pbkdf2Iterations = 210_000
	pbkdf2SaltLen    = 16
	pbkdf2KeyLen     = 32
	pbkdf2Prefix     = "pbkdf2-sha256"
)

// ErrPasswordMalformed 表示存储的哈希串格式不合法。
var ErrPasswordMalformed = errors.New("panelauth: malformed password hash")

// HashPassword 生成标准格式的密码哈希串：
//
//	pbkdf2-sha256$<iterations>$<salt-base64url>$<derive-key-base64url>
//
// 盐随机（每次调用不同），因此相同密码的哈希串不同。
func HashPassword(password string) (string, error) {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("panelauth: gen salt: %w", err)
	}
	return encodeHash(pbkdf2Iterations, salt, pbkdf2SHA256([]byte(password), salt, pbkdf2Iterations, pbkdf2KeyLen)), nil
}

// hashWithIters 供测试用的低迭代版本（避免单测被 21 万次迭代拖慢）。
func hashWithIters(password string, iters int) (string, error) {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return encodeHash(iters, salt, pbkdf2SHA256([]byte(password), salt, iters, pbkdf2KeyLen)), nil
}

func encodeHash(iters int, salt, dk []byte) string {
	return fmt.Sprintf("%s$%d$%s$%s", pbkdf2Prefix, iters,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk))
}

// VerifyPassword 常量时间校验 password 是否匹配 hash。
// 任何格式问题一律返回 false（把"格式坏"和"密码错"对外统一，避免泄露存储细节）。
func VerifyPassword(hash, password string) bool {
	dk, err := deriveFromHash(hash, []byte(password))
	if err != nil {
		return false
	}
	return dk != nil
}

// dummyVerify 对一个固定哈希做一次真实派生，用于"用户不存在"路径，
// 让登录耗时与"用户存在但密码错"一致，避免用户名枚举旁路。
var dummyHash = func() string {
	h, err := hashWithIters("panelauth-dummy", pbkdf2Iterations)
	if err != nil {
		return ""
	}
	return h
}()

func dummyVerify(password string) {
	_ = VerifyPassword(dummyHash, password)
}

// deriveFromHash 解析 hash 并按其中记录的参数重新派生，返回校验结果。
// 匹配返回非 nil 派生值，不匹配/格式错返回 nil + err（不匹配时 err 为 ErrPasswordMalformed
// 之外的 nil，用 nil,nil 表示"格式合法但密码错"）。
func deriveFromHash(hash string, password []byte) ([]byte, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != pbkdf2Prefix {
		return nil, ErrPasswordMalformed
	}
	iters, err := strconv.Atoi(parts[1])
	if err != nil || iters <= 0 || iters > 10_000_000 {
		return nil, ErrPasswordMalformed
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return nil, ErrPasswordMalformed
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return nil, ErrPasswordMalformed
	}
	got := pbkdf2SHA256(password, salt, iters, len(want))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return nil, nil // 格式合法但密码不匹配
	}
	return got, nil
}

// pbkdf2SHA256 实现 PBKDF2-HMAC-SHA256（RFC 8018 §5.2）。
// 与 golang.org/x/crypto/pbkdf2 同构：按块迭代 U_i，T = U_1 xor ... xor U_c。
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen

	var buf [4]byte
	dk := make([]byte, 0, numBlocks*hashLen)
	u := make([]byte, hashLen)
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Write(buf[:])
		u = prf.Sum(u[:0])
		t := make([]byte, hashLen)
		copy(t, u)
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for i := range t {
				t[i] ^= u[i]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}
