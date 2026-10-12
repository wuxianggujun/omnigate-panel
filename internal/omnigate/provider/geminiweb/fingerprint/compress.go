package fingerprint

import (
	"bufio"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// DecompressTransport 包一层 RoundTripper：当响应带 Content-Encoding
// （br / zstd / gzip / deflate）时，把 Body 换成解码后的流。
//
// 为什么需要它：指纹请求头里声明了 "gzip, deflate, br, zstd"，而 Go 的 net/http
// 只在「由它自己添加 Accept-Encoding」时才自动解 gzip。调用方一旦手写了这个头，
// 就必须自行解码，否则拿到的是一串压缩字节。
type DecompressTransport struct {
	Base http.RoundTripper
}

// RoundTrip 实现 http.RoundTripper。
func (d DecompressTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := d.Base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" || resp.Body == nil {
		return resp, nil
	}

	// 先缓冲再解码：解码器构造失败时，body 尚未被消费，可以原样还给调用方
	// （例如服务器在 Content-Encoding: gzip 下塞了一页纯文本错误）。Peek 只
	// 看前缀、不推进读指针，因此「校验失败」路径下字节一个都不会丢。
	br := bufio.NewReader(resp.Body)

	var body io.ReadCloser
	switch enc {
	case "gzip", "x-gzip":
		if !looksGzip(br) {
			return passthrough(resp, br), nil
		}
		zr, zerr := gzip.NewReader(br)
		if zerr != nil {
			return passthrough(resp, br), nil
		}
		body = &wrappedReadCloser{Reader: zr, closers: []io.Closer{zr, resp.Body}}
	case "deflate":
		if !looksZlib(br) {
			return passthrough(resp, br), nil
		}
		zr, zerr := zlib.NewReader(br)
		if zerr != nil {
			return passthrough(resp, br), nil
		}
		body = &wrappedReadCloser{Reader: zr, closers: []io.Closer{zr, resp.Body}}
	case "br":
		// brotli 无固定魔数，只能交给解码器；坏流会在 Read 时报错。
		body = &wrappedReadCloser{Reader: brotli.NewReader(br), closers: []io.Closer{resp.Body}}
	case "zstd":
		if !looksZstd(br) {
			return passthrough(resp, br), nil
		}
		zr, zerr := zstd.NewReader(br)
		if zerr != nil {
			return passthrough(resp, br), nil
		}
		rc := zr.IOReadCloser()
		body = &wrappedReadCloser{Reader: rc, closers: []io.Closer{rc, resp.Body}}
	default:
		return resp, nil
	}

	resp.Body = body
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return resp, nil
}

// passthrough 在解码不可用时把（未被消费的）缓冲流原样装回响应体。
func passthrough(resp *http.Response, br *bufio.Reader) *http.Response {
	resp.Body = &wrappedReadCloser{Reader: br, closers: []io.Closer{resp.Body}}
	return resp
}

// looksGzip 检查 gzip 魔数 0x1f 0x8b。
func looksGzip(br *bufio.Reader) bool {
	b, err := br.Peek(2)
	return err == nil && b[0] == 0x1f && b[1] == 0x8b
}

// looksZlib 检查 zlib 头：低 4 位为 8（deflate），且 (CMF<<8|FLG) 能被 31 整除。
func looksZlib(br *bufio.Reader) bool {
	b, err := br.Peek(2)
	if err != nil {
		return false
	}
	cmf, flg := int(b[0]), int(b[1])
	return cmf&0x0f == 8 && (cmf<<8|flg)%31 == 0
}

// looksZstd 检查 zstd 魔数 0x28 0xb5 0x2f 0xfd（小端 0xFD2FB528）。
func looksZstd(br *bufio.Reader) bool {
	b, err := br.Peek(4)
	return err == nil && b[0] == 0x28 && b[1] == 0xb5 && b[2] == 0x2f && b[3] == 0xfd
}

// wrappedReadCloser 让解码流关闭时顺带关闭底层响应体。
type wrappedReadCloser struct {
	io.Reader
	closers []io.Closer
}

func (w *wrappedReadCloser) Close() error {
	var first error
	for _, c := range w.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
