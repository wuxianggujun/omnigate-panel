package imageintent

import "testing"

// TestIsImageGeneration pins the boundaries of the strict check: the curated
// phrases match, the draw-a idiom set does not, and a bare "generate/create"
// without an image noun never does.
func TestIsImageGeneration(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"画一张猫的图片", true},
		{"来张图", true},
		{"draw a cat", true},
		{"generate an image of a dog", true},
		{"draw a conclusion", false},
		{"draw a map of the kingdom", false}, // idiom set wins: the safe side
		{"生成一份报告", false},
		{"create a plan", false},
		{"", false},
	} {
		if got := IsImageGeneration(tc.text); got != tc.want {
			t.Errorf("IsImageGeneration(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

// TestMaybeImageGeneration: the loose check must catch a strict hit even when the
// noun/verb pair is absent (bare "绘制"), and must stay generous on wording the
// strict list misses.
func TestMaybeImageGeneration(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"绘制", true},        // strict hit, no noun+verb pair
		{"帮我搞一张海报出来", true}, // noun + verb, not in the curated list
		{"帮我画个 logo", true}, // noun + verb
		{"what is the capital of France", false},
		{"", false},
		{"   ", false},
	} {
		if got := MaybeImageGeneration(tc.text); got != tc.want {
			t.Errorf("MaybeImageGeneration(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}
