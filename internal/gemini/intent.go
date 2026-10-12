package gemini

import (
	"regexp"
	"strings"
)

// Image-generation intent detection, ported from the upstream router. It exists
// because advertising tools in the prompt suppresses image generation: when the
// user is plainly asking for a picture, the client's tool declarations are
// dropped so the model draws instead of replying with a tool call.
//
// The asymmetry is deliberate (and upstream-tuned): a false positive silently
// drops the caller's tools — an agent loop breaks with no error and no log — so
// the lists below err toward excluding more. A false negative merely injects a
// tool prompt; the user notices and retries with clearer wording.
//
// Only the last user turn is ever passed here (see lastUserText): a flattened
// conversation would contain system prompts, history and tool results, and the
// word "image" inside a tool result is not the user asking for a picture.
var (
	// imageIntentCJK is matched as a plain substring: Chinese has no word
	// boundaries, so \b is meaningless there.
	imageIntentCJK = []string{
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

	// imageIntentASCII is matched with word boundaries. "draw a" / "draw an" are
	// handled separately (see drawArticleRE).
	imageIntentASCII = []string{
		"generate an image", "generate a picture",
		"generate an picture", "generate a poster", "create an image", "create a picture",
		"create a photo", "create a poster", "design a poster", "make an image",
		"make a picture", "make a poster", "an image of", "a picture of", "a photo of",
		"a poster of", "image of a", "picture of a", "poster of",
	}

	// drawIdiomObjects is the closed set of idiom objects that follow "draw a/an"
	// without asking for a picture ("draw a conclusion/distinction/line/…").
	// map/card/line stay in: they are also real drawable things, but the cost of
	// dropping a caller's tools (false positive) is far worse than missing one
	// image request (false negative).
	drawIdiomObjects = map[string]bool{
		"conclusion": true, "conclusions": true, "distinction": true, "distinctions": true,
		"parallel": true, "parallels": true, "line": true, "lines": true,
		"inference": true, "inferences": true, "outline": true, "outlines": true,
		"comparison": true, "comparisons": true, "analogy": true, "analogies": true,
		"attention": true, "blank": true, "blanks": true, "breath": true,
		"card": true, "cards": true, "salary": true, "crowd": true, "crowds": true,
		"map": true, "maps": true,
	}

	imageIntentASCIIRE = buildASCIIIntentRE()

	// drawArticleRE matches "draw a"/"draw an" and captures the next word. The
	// upstream uses a negative lookahead for the idiom set; RE2 has no lookahead,
	// so the exclusion is done by inspecting the captured word instead.
	drawArticleRE = regexp.MustCompile(`\bdraw an?\b\s*([A-Za-z]*)`)
)

func buildASCIIIntentRE() *regexp.Regexp {
	alts := make([]string, 0, len(imageIntentASCII))
	for _, p := range imageIntentASCII {
		alts = append(alts, regexp.QuoteMeta(p))
	}
	if len(alts) == 0 {
		// Never match. Compiling "" would match everything and turn the intent
		// check into an unconditional tool-dropper.
		return regexp.MustCompile(`\b\B`)
	}
	return regexp.MustCompile(`\b(?:` + strings.Join(alts, `|`) + `)\b`)
}

// isImageGenerationIntent reports whether text is a plain "draw me a picture"
// request. text must be the last user turn's text only.
func isImageGenerationIntent(text string) bool {
	if text == "" {
		return false
	}
	low := strings.ToLower(text)
	for _, p := range imageIntentCJK {
		if strings.Contains(low, p) {
			return true
		}
	}
	if imageIntentASCIIRE.MatchString(low) {
		return true
	}
	for _, m := range drawArticleRE.FindAllStringSubmatch(low, -1) {
		if len(m) < 2 || m[1] == "" || !drawIdiomObjects[m[1]] {
			return true
		}
	}
	return false
}
