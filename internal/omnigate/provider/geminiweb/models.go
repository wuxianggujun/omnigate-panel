// Package geminiweb adapts the Gemini Web (gemini.google.com) frontend protocol
// to the omnigate provider interface. It speaks the same private endpoints the
// browser app uses, authenticated by the account's Google cookies.
package geminiweb

import (
	"encoding/json"
	"strconv"
)

// modelInfo is one internal Gemini Web model. ID and ModelNumber are wire values
// the upstream matches on, so they are reproduced verbatim; everything else is
// bookkeeping.
type modelInfo struct {
	ID          string
	Capacity    int
	ProOnly     bool
	Family      string
	ModelNumber int
}

// modelCatalog maps internal model names to their wire metadata.
var modelCatalog = map[string]modelInfo{
	"gemini-3-pro":                     {ID: "9d8ca3786ebdfbea", Capacity: 1, Family: "pro", ModelNumber: 3},
	"gemini-3-flash":                   {ID: "fbb127bbb056c959", Capacity: 1, Family: "flash", ModelNumber: 1},
	"gemini-3-flash-thinking":          {ID: "5bf011840784117a", Capacity: 1, Family: "flash-thinking", ModelNumber: 1},
	"gemini-3-flash-lite":              {ID: "cf41b0e0dd7d53e5", Capacity: 1, Family: "flash-lite", ModelNumber: 6},
	"gemini-3-pro-plus":                {ID: "e6fa609c3fa255c0", Capacity: 4, ProOnly: true, Family: "pro", ModelNumber: 3},
	"gemini-3-flash-plus":              {ID: "56fdd199312815e2", Capacity: 4, ProOnly: true, Family: "flash", ModelNumber: 1},
	"gemini-3-flash-thinking-plus":     {ID: "e051ce1aa80aa576", Capacity: 4, ProOnly: true, Family: "flash-thinking", ModelNumber: 1},
	"gemini-3-flash-lite-plus":         {ID: "8c46e95b1a07cecc", Capacity: 4, ProOnly: true, Family: "flash-lite", ModelNumber: 6},
	"gemini-3-pro-advanced":            {ID: "e6fa609c3fa255c0", Capacity: 2, ProOnly: true, Family: "pro", ModelNumber: 3},
	"gemini-3-flash-advanced":          {ID: "56fdd199312815e2", Capacity: 2, ProOnly: true, Family: "flash", ModelNumber: 1},
	"gemini-3-flash-thinking-advanced": {ID: "e051ce1aa80aa576", Capacity: 2, ProOnly: true, Family: "flash-thinking", ModelNumber: 1},
	"gemini-3-flash-lite-advanced":     {ID: "8c46e95b1a07cecc", Capacity: 2, ProOnly: true, Family: "flash-lite", ModelNumber: 6},
}

// publicModels are the names the gateway exposes. They never change, so an
// account's subscription tier or Google's rollout can shift underneath without
// breaking the API contract.
var publicModels = []string{"gemini-pro", "gemini-flash", "gemini-flash-thinking", "gemini-flash-lite"}

var publicFamily = map[string]string{
	"gemini-pro":            "pro",
	"gemini-flash":          "flash",
	"gemini-flash-thinking": "flash-thinking",
	"gemini-flash-lite":     "flash-lite",
}

// familyDefault is the internal model used for a family when the account has not
// reported which concrete model it currently serves (base tier).
var familyDefault = map[string]string{
	"pro":            "gemini-3-pro",
	"flash":          "gemini-3-flash",
	"flash-thinking": "gemini-3-flash-thinking",
	"flash-lite":     "gemini-3-flash-lite",
}

// modelAliases maps legacy names onto the stable public names.
var modelAliases = map[string]string{
	"gemini-2.5-pro":                 "gemini-pro",
	"gemini-2.5-flash":               "gemini-flash",
	"gemini-2.5-flash-thinking":      "gemini-flash-thinking",
	"gemini-2.5-pro-preview-05-06":   "gemini-pro",
	"gemini-2.5-flash-preview-04-17": "gemini-flash",
	"gemini-2.5-flash-preview-05-20": "gemini-flash",
	"gemini-2.0-flash":               "gemini-flash",
	"gemini-2.0-flash-thinking":      "gemini-flash-thinking",
	"gemini-2.0-flash-lite":          "gemini-flash-lite",
	"gemini-1.5-pro":                 "gemini-pro",
	"gemini-1.5-flash":               "gemini-flash",
}

// resolveModel maps a client-supplied model name to the internal model this
// account should use. familyModel is the per-account family→internal map learned
// from the status RPC; nil (or a miss) falls back to the family default.
func resolveModel(name string, familyModel map[string]string) string {
	if a, ok := modelAliases[name]; ok {
		name = a
	}
	fam, ok := publicFamily[name]
	if !ok {
		return name
	}
	if m := familyModel[fam]; m != "" {
		return m
	}
	return familyDefault[fam]
}

// modelNumber returns the resolved model's model-type code (pro=3, flash=1,
// flash-lite=6); unknown names default to 1.
func modelNumber(name string) int {
	if info, ok := modelCatalog[resolveModel(name, nil)]; ok {
		return info.ModelNumber
	}
	return 1
}

// idToName maps internal wire ids back to internal model names.
func idToName() map[string]string {
	out := make(map[string]string, len(modelCatalog))
	for name, info := range modelCatalog {
		out[info.ID] = name
	}
	return out
}

// buildModelHeader returns the model-selection headers for the standard path.
func buildModelHeader(name string) map[string]string {
	info, ok := modelCatalog[resolveModel(name, nil)]
	if !ok {
		return nil
	}
	return map[string]string{
		modelHeaderKey: "[1,null,null,null,\"" + info.ID + "\",null,null,0,[4],null,null," +
			strconv.Itoa(info.Capacity) + "]",
		"x-goog-ext-73010989-jspb": "[0]",
		"x-goog-ext-73010990-jspb": "[0]",
	}
}

// buildModelHeaderThinking returns the model-selection headers for the
// extended-thinking path: a 17-element array whose trailing flag is 2.
func buildModelHeaderThinking(name, sessionUUID string) map[string]string {
	info, ok := modelCatalog[resolveModel(name, nil)]
	if !ok {
		return nil
	}
	arr := []any{
		1, nil, nil, nil, info.ID, nil, nil, 0, []any{4, 5, 6, 8},
		nil, nil, info.Capacity, nil, nil, info.ModelNumber,
		2, // 2 = extended thinking (1 = standard)
		sessionUUID,
	}
	b, err := json.Marshal(arr)
	if err != nil {
		return nil
	}
	return map[string]string{
		modelHeaderKey:             string(b),
		"x-goog-ext-73010989-jspb": "[0]",
		"x-goog-ext-73010990-jspb": "[0,0,0]",
	}
}
