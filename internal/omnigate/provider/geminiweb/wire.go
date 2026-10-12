package geminiweb

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Private endpoints the Gemini Web frontend talks to.
const (
	geminiAppURL     = "https://gemini.google.com/app"
	geminiHomeURL    = "https://gemini.google.com/?hl=en"
	geminiAppENURL   = "https://gemini.google.com/app?hl=en"
	googleHomeURL    = "https://www.google.com/"
	rotateCookiesURL = "https://accounts.google.com/RotateCookies"
	generateURL      = "https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate"
	batchexecuteURL  = "https://gemini.google.com/_/BardChatUi/data/batchexecute"

	modelHeaderKey = "x-goog-ext-525001261-jspb"

	// defaultPushID is the upload push id used when the app page does not expose
	// one.
	defaultPushID = "feeds/mcudyrk2a4khkz"
)

// fileRef is one uploaded attachment: its wire id plus the display name the
// frontend echoes back.
type fileRef struct {
	ID   string
	Name string
}

// encodePayload builds the f.req body for a standard (non-thinking) turn.
func encodePayload(prompt, model, conversationID string, files []fileRef, gemID string) string {
	var conv any
	if conversationID != "" {
		conv = conversationID
	}
	var content any
	if len(files) > 0 {
		fileData := make([]any, 0, len(files))
		for _, f := range files {
			fileData = append(fileData, []any{[]any{f.ID}, f.Name})
		}
		content = []any{prompt, 0, nil, fileData, nil, nil, 0}
	} else {
		content = []any{prompt}
	}
	inner := []any{content, nil, conv, model}
	if gemID != "" {
		for len(inner) < 20 {
			inner = append(inner, nil)
		}
		inner[19] = gemID
	}
	innerJSON, _ := json.Marshal(inner)
	outer, _ := json.Marshal([]any{nil, string(innerJSON)})
	return string(outer)
}

// encodePayloadThinking builds the f.req body for the extended-thinking path.
func encodePayloadThinking(prompt, model, conversationID string, files []fileRef, gemID, sessionUUID string) string {
	var content any
	if len(files) > 0 {
		fileData := make([]any, 0, len(files))
		for _, f := range files {
			fileData = append(fileData, []any{[]any{f.ID}, f.Name})
		}
		content = []any{prompt, 0, nil, fileData, nil, nil, 0}
	} else {
		content = []any{prompt, 0, nil, nil, nil, nil, 0}
	}
	inner := make([]any, 81)
	inner[0] = content
	inner[1] = []any{"en"}
	inner[2] = []any{"", "", "", nil, nil, nil, nil, nil, nil, ""}
	inner[6] = []any{1}
	inner[7] = 1
	inner[10] = 1
	inner[11] = 0
	inner[17] = []any{[]any{0}}
	inner[18] = 0
	if gemID != "" {
		inner[19] = gemID
	}
	inner[27] = 1
	inner[30] = []any{4}
	inner[41] = []any{1}
	inner[53] = 0
	inner[59] = sessionUUID
	inner[61] = []any{}
	inner[68] = 1
	inner[79] = modelNumber(model)
	inner[80] = 2 // 2 = extended thinking
	innerJSON, _ := json.Marshal(inner)
	outer, _ := json.Marshal([]any{nil, string(innerJSON)})
	return string(outer)
}

// scanCompleteWrbFrames slices every complete top-level ["wrb.fr",...] element
// out of a growing response buffer. StreamGenerate streams a JSON array whose
// elements arrive across arbitrary chunk boundaries, so the buffer is scanned by
// bracket depth (string- and escape-aware) and only fully closed elements are
// returned; the returned offset is how much of the buffer was consumed.
func scanCompleteWrbFrames(buf string) (frames []any, consumed int) {
	i, n := 0, len(buf)
	for i < n {
		start := strings.Index(buf[i:], `["wrb.fr"`)
		if start == -1 {
			break
		}
		start += i
		depth, inStr, esc, end := 0, false, false, -1
		for j := start; j < n; j++ {
			c := buf[j]
			if inStr {
				switch {
				case esc:
					esc = false
				case c == '\\':
					esc = true
				case c == '"':
					inStr = false
				}
				continue
			}
			switch c {
			case '"':
				inStr = true
			case '[':
				depth++
			case ']':
				depth--
				if depth == 0 {
					end = j
				}
			}
			if end != -1 {
				break
			}
		}
		if end == -1 {
			break
		}
		var elem any
		if err := json.Unmarshal([]byte(buf[start:end+1]), &elem); err == nil {
			frames = append(frames, elem)
		}
		consumed = end + 1
		i = end + 1
	}
	return frames, consumed
}

// extractTextFromWrb pulls the cumulative text, conversation id and thinking
// chain out of one wrb.fr frame. hasText is false for frames that carry no text
// candidate.
func extractTextFromWrb(elem any) (text string, hasText bool, conv string, thoughts string) {
	arr, ok := elem.([]any)
	if !ok || len(arr) < 3 {
		return
	}
	if s, _ := arr[0].(string); s != "wrb.fr" {
		return
	}
	raw, ok := arr[2].(string)
	if !ok {
		return
	}
	var payload []any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil || len(payload) < 5 {
		return
	}
	conv = jsonScalarString(payload[1])
	cands, ok := payload[4].([]any)
	if !ok || len(cands) == 0 {
		return
	}
	c0, ok := cands[0].([]any)
	if !ok {
		return
	}
	if len(c0) > 1 {
		if parts, ok := c0[1].([]any); ok && len(parts) > 0 {
			if s, ok := parts[0].(string); ok {
				text, hasText = s, true
			}
		}
	}
	if len(c0) > 37 {
		if a, ok := c0[37].([]any); ok && len(a) > 0 {
			if b, ok := a[0].([]any); ok && len(b) > 0 {
				if s, ok := b[0].(string); ok {
					thoughts = s
				}
			}
		}
	}
	return
}

// extractImagesFromWrb pulls the AI-generated images out of one wrb.fr frame,
// reusing extractGeneratedImages. Empty for frames that carry none.
func extractImagesFromWrb(elem any) []genImage {
	arr, ok := elem.([]any)
	if !ok || len(arr) < 3 {
		return nil
	}
	if s, _ := arr[0].(string); s != "wrb.fr" {
		return nil
	}
	raw, ok := arr[2].(string)
	if !ok {
		return nil
	}
	var payload []any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil || len(payload) < 5 {
		return nil
	}
	cands, ok := payload[4].([]any)
	if !ok || len(cands) == 0 {
		return nil
	}
	candidate, ok := cands[0].([]any)
	if !ok {
		return nil
	}
	return extractGeneratedImages(candidate)
}

// genResult is the non-streaming result of one turn.
type genResult struct {
	Text     string
	Thoughts string
	ConvID   string
	Images   []genImage
}

// genImage is one AI-generated image found in a response frame.
type genImage struct {
	URL      string
	Mime     string
	Width    int
	Height   int
	Filename string
}

// noImageHint replaces an empty reply after a placeholder URL was filtered out:
// Gemini answered a vague "来张X图" by *retrieving* an image rather than
// generating one, and the retrieval placeholder is useless to a client. Telling
// the user how to ask again beats returning nothing.
const noImageHint = "（没有生成图片。试试更明确的生成指令，如「画一张…」「生成一张…的图片」。）"

// extractGeneratedImages pulls the images the model generated out of one
// candidate. This is not where a user's own attachments live; it reads
// candidate[12][7][0], a list of images where each image's metadata is a list of
// the form [null, 1, filename, url, …, "image/png", …, [w, h, size]].
//
// The upstream reaches that metadata as img[0][3], i.e. the image is itself
// wrapped in a list. The wrapper is read first and skipped when absent, because
// this shape is only ever observed from live traffic and a misread nesting would
// make image extraction fail silently — the one failure mode nothing else here
// would catch.
func extractGeneratedImages(candidate []any) []genImage {
	if len(candidate) <= 12 {
		return nil
	}
	c12, ok := candidate[12].([]any)
	if !ok || len(c12) <= 7 {
		return nil
	}
	arr, ok := c12[7].([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	imgList, ok := arr[0].([]any)
	if !ok {
		return nil
	}
	var out []genImage
	for _, raw := range imgList {
		img, ok := raw.([]any)
		if !ok || len(img) == 0 {
			continue
		}
		meta, ok := imageMeta(img[0])
		if !ok {
			continue
		}
		url, _ := meta[3].(string)
		if !strings.HasPrefix(url, "http") {
			continue
		}
		g := genImage{URL: url, Mime: "image/png", Filename: "generated.png"}
		if fn, ok := meta[2].(string); ok && fn != "" {
			g.Filename = fn
		}
		for _, el := range meta {
			if s, ok := el.(string); ok && strings.HasPrefix(s, "image/") {
				g.Mime = s
				continue
			}
			if pair, ok := el.([]any); ok && len(pair) >= 2 {
				if w, ok := numAsInt(pair[0]); ok {
					if h, ok2 := numAsInt(pair[1]); ok2 {
						g.Width, g.Height = w, h
					}
				}
			}
		}
		out = append(out, g)
	}
	return out
}

// imageMeta resolves an image entry's metadata list, which is either the entry's
// first element (img[0]) or that element's fourth (img[0][3]).
func imageMeta(first any) ([]any, bool) {
	outer, ok := first.([]any)
	if !ok {
		return nil, false
	}
	if len(outer) > 3 {
		if inner, ok := outer[3].([]any); ok {
			return inner, true
		}
	}
	return outer, true
}

// numAsInt reads a JSON number as an int (encoding/json decodes every number as
// float64).
func numAsInt(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
}

// cleanGeneratedText strips the meaningless image placeholder URLs the model
// emits alongside a real generated image. If the text was nothing but a
// placeholder and no image came back, it substitutes a hint instead of an empty
// reply.
func cleanGeneratedText(text string, hasImages bool) string {
	if !strings.Contains(text, "googleusercontent.com/image") {
		return text
	}
	hadPlaceholder := imageGenPlaceholderRE.MatchString(text)
	out := strings.TrimSpace(imageGenPlaceholderRE.ReplaceAllString(text, ""))
	if hadPlaceholder && out == "" && !hasImages {
		return noImageHint
	}
	return out
}

// parseOutput walks a non-streaming StreamGenerate body (a `)]}'` prefix plus
// newline-delimited JSON arrays) and returns the final text/thoughts/conv id.
func parseOutput(raw string) genResult {
	var out genResult
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ")]}'") {
			continue
		}
		var data []any
		if err := json.Unmarshal([]byte(line), &data); err != nil {
			continue
		}
		for _, item := range data {
			arr, ok := item.([]any)
			if !ok || len(arr) < 3 {
				continue
			}
			rawPayload, ok := arr[2].(string)
			if !ok {
				continue
			}
			var payload []any
			if err := json.Unmarshal([]byte(rawPayload), &payload); err != nil || len(payload) < 5 {
				continue
			}
			if c := jsonScalarString(payload[1]); c != "" {
				out.ConvID = c
			}
			cands, ok := payload[4].([]any)
			if !ok || len(cands) == 0 {
				continue
			}
			candidate, ok := cands[0].([]any)
			if !ok || len(candidate) <= 1 {
				continue
			}
			if parts, ok := candidate[1].([]any); ok && len(parts) > 0 {
				if s, ok := parts[0].(string); ok {
					out.Text = s
				}
			}
			if len(candidate) > 37 {
				if a, ok := candidate[37].([]any); ok && len(a) > 0 {
					if b, ok := a[0].([]any); ok && len(b) > 0 {
						if s, ok := b[0].(string); ok {
							out.Thoughts = s
						}
					}
				}
			}
			// 生图是流式响应，用含图的最新帧。
			if imgs := extractGeneratedImages(candidate); len(imgs) > 0 {
				out.Images = imgs
			}
		}
	}
	out.Text = cleanGeneratedText(out.Text, len(out.Images) > 0)
	return out
}

// parseWrbBody returns the decoded body of the first wrb.fr frame in a
// batchexecute response whose rpc id is rpcID (or unset). nil when absent.
func parseWrbBody(raw, rpcID string) any {
	if raw == "" {
		return nil
	}
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[[") {
			continue
		}
		var outer []any
		if err := json.Unmarshal([]byte(line), &outer); err != nil {
			continue
		}
		for _, item := range outer {
			arr, ok := item.([]any)
			if !ok || len(arr) < 3 {
				continue
			}
			if s, _ := arr[0].(string); s != "wrb.fr" {
				continue
			}
			if arr[1] != nil {
				if s, ok := arr[1].(string); !ok || s != rpcID {
					continue
				}
			}
			bodyStr, ok := arr[2].(string)
			if !ok {
				continue
			}
			var body any
			if err := json.Unmarshal([]byte(bodyStr), &body); err != nil {
				continue
			}
			return body
		}
	}
	return nil
}

// parseModelsFromStatus reads the account's real per-family models out of an
// otAQ7b (GetUserStatus) response: body[15] lists the enabled model ids.
func parseModelsFromStatus(raw string) map[string]string {
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[[") {
			continue
		}
		var outer []any
		if err := json.Unmarshal([]byte(line), &outer); err != nil {
			continue
		}
		for _, item := range outer {
			arr, ok := item.([]any)
			if !ok || len(arr) < 3 {
				continue
			}
			if s, _ := arr[0].(string); s != "wrb.fr" {
				continue
			}
			bodyStr, ok := arr[2].(string)
			if !ok {
				continue
			}
			var body []any
			if err := json.Unmarshal([]byte(bodyStr), &body); err != nil || len(body) <= 15 {
				continue
			}
			modelsList, ok := body[15].([]any)
			if !ok {
				continue
			}
			var discovered []string
			for _, m := range modelsList {
				if ma, ok := m.([]any); ok && len(ma) > 0 {
					if s, ok := ma[0].(string); ok {
						discovered = append(discovered, s)
					}
				}
			}
			byID := idToName()
			familyModel := map[string]string{}
			for _, mid := range discovered {
				name := byID[mid]
				if name == "" {
					continue
				}
				fam := modelCatalog[name].Family
				if _, ok := familyModel[fam]; !ok {
					familyModel[fam] = name
				}
			}
			if len(familyModel) == 0 {
				continue
			}
			// Fill the remaining families at the same capacity tier (a pro/flash
			// hit implies the matching thinking model is available too).
			caps := map[int]bool{}
			for _, n := range familyModel {
				caps[modelCatalog[n].Capacity] = true
			}
			for name, info := range modelCatalog {
				if caps[info.Capacity] {
					if _, ok := familyModel[info.Family]; !ok {
						familyModel[info.Family] = name
					}
				}
			}
			return familyModel
		}
	}
	return nil
}

// Placeholder URLs the model emits for generated/retrieved images. They are
// meaningless to clients (the real image lives in the images field), so they are
// stripped from text.
var imageGenPlaceholderRE = regexp.MustCompile(
	`https?://googleusercontent\.com/(?:image_generation_content|image_retrieval|image_collection)[/\w]*\d*`)

const (
	imagePlaceholderPrefix  = "http://googleusercontent.com/"
	imagePlaceholderPrefixS = "https://googleusercontent.com/"
)

// stablePlaceholderPrefix returns the portion of text that is safe to emit
// while streaming: complete placeholder URLs are stripped, and a trailing
// fragment that could still grow into one is held back so a half-formed URL is
// never streamed to the client.
func stablePlaceholderPrefix(text string) string {
	cleaned := imageGenPlaceholderRE.ReplaceAllString(text, "")
	cut := len(cleaned)
	for from := 0; ; {
		p := strings.Index(cleaned[from:], "http")
		if p == -1 {
			break
		}
		p += from
		tail := cleaned[p:]
		for _, tmpl := range []string{imagePlaceholderPrefix, imagePlaceholderPrefixS} {
			if strings.HasPrefix(tmpl, tail) || strings.HasPrefix(tail, tmpl) {
				if p < cut {
					cut = p
				}
				break
			}
		}
		from = p + 1
	}
	for _, k := range []int{3, 2, 1} {
		if cut == len(cleaned) && strings.HasSuffix(cleaned, "http"[:k]) {
			cut = len(cleaned) - k
			break
		}
	}
	return cleaned[:cut]
}

var modelValidRE = regexp.MustCompile(
	`^gemini-(\d+)\.(\d+)-([a-z]+)(?:-([a-z]+))?(?:-preview)?(?:-\d{2}-\d{2})?$`)

// filterValidModels keeps only well-formed model names, deduplicated and sorted.
func filterValidModels(hits []string) []string {
	seen := map[string]bool{}
	var valid []string
	for _, m := range hits {
		if seen[m] || !modelValidRE.MatchString(m) {
			continue
		}
		if len(m) < 12 || len(m) > 50 {
			continue
		}
		if len(strings.Split(m, "-")) > 7 {
			continue
		}
		seen[m] = true
		valid = append(valid, m)
	}
	sort.Strings(valid)
	return valid
}

// jsonScalarString renders a JSON scalar the way Python's str() would, so
// conversation ids stay byte-identical between the streaming and non-streaming
// paths.
func jsonScalarString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}
