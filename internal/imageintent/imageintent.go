// Package imageintent decides whether a user message is asking for a generated
// image. Two strengths are exported because they guard two very different costs:
//
//   - IsImageGeneration is the strict check. It decides whether a client's tool
//     declarations get dropped, because advertising tools in the prompt
//     suppresses image generation. A false positive here silently breaks an agent
//     loop (tools vanish, no error, no log), so the phrase list is curated and
//     errs toward excluding more.
//   - MaybeImageGeneration is the loose check. It decides whether a request is
//     buffered instead of streamed, because a generated image's link has to be
//     prepended to the text and the text is only complete at the end. A false
//     positive costs a slower first byte; a false negative loses the image. So it
//     is deliberately generous: a curated strict hit, or any image noun next to
//     any production verb.
//
// Both are ported from the upstream's app/utils/tools.py, including the idiom
// exclusion for "draw a conclusion/distinction/…", which exists because a bare
// "draw a" reads like a picture request.
//
// Callers must pass the last user turn's text only — never a flattened
// conversation, which contains system prompts, history and tool results where the
// word "image" means something else entirely.
package imageintent

import (
	"regexp"
	"strings"
)

var (
	// cjkPatterns are matched as plain substrings: Chinese has no word
	// boundaries, so \b is meaningless there.
	cjkPatterns = []string{
		"画一", "画个", "画张", "画幅", "画副", "帮我画", "给我画", "画图", "画一张", "画一幅",
		"画个图", "画张图", "画幅图", "画一只", "画只", "画头", "画条",
		"生成图片", "生成一张图", "生成图像", "生成一幅", "生成一张照片", "生成张图", "生成个图",
		"生成海报", "生成插画", "生成一张海报",
		"做一张图", "做个图", "做张图", "做一张海报", "做个海报", "做张海报", "做一张照片",
		"设计一张海报", "设计海报", "设计一张图", "设计张海报", "设计个海报", "设计一幅",
		"画一张海报", "画张海报", "画个海报", "画海报",
		"绘制", "绘一", "画出", "出一张图", "出张图", "p一张", "p个图", "做幅",
		"一张海报", "张海报", "幅海报", "来张图", "来一张图", "来个图",
	}

	// asciiPatterns are matched with word boundaries. "draw a" / "draw an" are
	// handled separately (see drawArticleRE).
	asciiPatterns = []string{
		"generate an image", "generate a picture",
		"generate an picture", "generate a poster", "create an image", "create a picture",
		"create a photo", "create a poster", "design a poster", "make an image",
		"make a picture", "make a poster", "an image of", "a picture of", "a photo of",
		"a poster of", "image of a", "picture of a", "poster of",
	}

	// drawIdiomObjects is the closed set of objects that follow "draw a/an"
	// without asking for a picture. map/card/line stay in even though they are
	// also drawable things: dropping a caller's tools is far worse than missing
	// one image request.
	drawIdiomObjects = map[string]bool{
		"conclusion": true, "conclusions": true, "distinction": true, "distinctions": true,
		"parallel": true, "parallels": true, "line": true, "lines": true,
		"inference": true, "inferences": true, "outline": true, "outlines": true,
		"comparison": true, "comparisons": true, "analogy": true, "analogies": true,
		"attention": true, "blank": true, "blanks": true, "breath": true,
		"card": true, "cards": true, "salary": true, "crowd": true, "crowds": true,
		"map": true, "maps": true,
	}

	asciiRE = buildASCII()

	// drawArticleRE matches "draw a"/"draw an" and captures the next word. The
	// upstream uses a negative lookahead for the idiom set; Go's RE2 has no
	// lookahead, so the exclusion is done by inspecting the captured word.
	drawArticleRE = regexp.MustCompile(`\bdraw an?\b\s*([A-Za-z]*)`)

	// imgNouns / imgVerbs feed the loose check.
	imgNouns = []string{"图", "图片", "图像", "海报", "插画", "照片", "壁纸", "logo", "头像", "封面",
		"image", "picture", "poster", "photo", "drawing", "illustration", "wallpaper", "avatar",
		"draw a", "draw an"}
	imgVerbs = []string{"画", "生成", "绘", "做", "设计", "出", "整", "来", "搞", "弄", "制作", "帮我", "给我",
		"想要", "要", "想", "需要", "求", "来一", "来个", "来张",
		"draw", "generate", "create", "make", "design", "render", "want", "need"}
)

func buildASCII() *regexp.Regexp {
	alts := make([]string, 0, len(asciiPatterns))
	for _, p := range asciiPatterns {
		alts = append(alts, regexp.QuoteMeta(p))
	}
	if len(alts) == 0 {
		// Never match. Compiling "" would match everything and turn the strict
		// check into an unconditional tool-dropper.
		return regexp.MustCompile(`\b\B`)
	}
	return regexp.MustCompile(`\b(?:` + strings.Join(alts, `|`) + `)\b`)
}

// IsImageGeneration reports whether text is plainly a "draw me a picture"
// request.
func IsImageGeneration(text string) bool {
	if text == "" {
		return false
	}
	low := strings.ToLower(text)
	for _, p := range cjkPatterns {
		if strings.Contains(low, p) {
			return true
		}
	}
	if asciiRE.MatchString(low) {
		return true
	}
	for _, m := range drawArticleRE.FindAllStringSubmatch(low, -1) {
		// m[1] is the following word (possibly empty). An idiom object means the
		// sentence is not about drawing; anything else — including nothing — is.
		if len(m) < 2 || m[1] == "" || !drawIdiomObjects[m[1]] {
			return true
		}
	}
	return false
}

// MaybeImageGeneration is the loose, safe-side check: a strict hit, or an image
// noun next to a production verb.
func MaybeImageGeneration(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	if IsImageGeneration(text) {
		return true
	}
	low := strings.ToLower(text)
	return containsAny(low, imgNouns) && containsAny(low, imgVerbs)
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
