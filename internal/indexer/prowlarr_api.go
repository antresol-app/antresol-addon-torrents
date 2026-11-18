package indexer

import (
	"encoding/json"
	"slices"
	"strings"
)

// maskedValue is what Prowlarr's API answers in place of a password
// (a field of privacy "password" or "apiKey").
const maskedValue = "********"

// ParseProwlarrAPI turns Prowlarr's answers to GET /api/v1/indexer and GET
// /api/v1/tag into the indexers ImportProwlarrList takes, in the shape the
// database rows have: a Cardigann indexer's fields become its
// definitionFile, baseUrl and extraFieldData, a native one's stay flat.
// Masked values (passwords) are left out and named in Hidden; a cookie
// comes as any other field. The live session cookies Prowlarr keeps
// (IndexerStatus) are not in its API.
func ParseProwlarrAPI(indexers, tags []byte) ([]ProwlarrIndexer, error) {
	var list []struct {
		ID             int64  `json:"id"`
		Name           string `json:"name"`
		Implementation string `json:"implementation"`
		Enable         *bool  `json:"enable"`
		Priority       *int   `json:"priority"`
		Tags           []int  `json:"tags"`
		Fields         []struct {
			Name  string          `json:"name"`
			Value json.RawMessage `json:"value"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(indexers, &list); err != nil {
		return nil, err
	}
	flare := map[int]bool{}
	if len(tags) > 0 {
		var ts []struct {
			ID    int    `json:"id"`
			Label string `json:"label"`
		}
		if err := json.Unmarshal(tags, &ts); err != nil {
			return nil, err
		}
		for _, t := range ts {
			if strings.EqualFold(strings.TrimSpace(t.Label), "flaresolverr") {
				flare[t.ID] = true
			}
		}
	}
	out := make([]ProwlarrIndexer, 0, len(list))
	for _, x := range list {
		p := ProwlarrIndexer{ID: x.ID, Name: x.Name, Implementation: x.Implementation, Enable: true, Priority: 25,
			FlareSolverr: slices.ContainsFunc(x.Tags, func(id int) bool { return flare[id] })}
		if x.Enable != nil {
			p.Enable = *x.Enable
		}
		if x.Priority != nil {
			p.Priority = *x.Priority
		}
		values := map[string]any{}
		for _, f := range x.Fields {
			if f.Name == "" || len(f.Value) == 0 || string(f.Value) == "null" {
				continue
			}
			var v any
			if json.Unmarshal(f.Value, &v) != nil {
				continue
			}
			if s, ok := v.(string); ok && s == maskedValue {
				p.Hidden = append(p.Hidden, f.Name)
				continue
			}
			values[f.Name] = v
		}
		var settings any = values
		if strings.EqualFold(x.Implementation, "cardigann") {
			c := map[string]any{"extraFieldData": map[string]any{}}
			extra := c["extraFieldData"].(map[string]any)
			for k, v := range values {
				switch k {
				case "definitionFile", "baseUrl":
					c[k] = v
				default:
					extra[k] = v
				}
			}
			settings = c
		}
		raw, err := json.Marshal(settings)
		if err != nil {
			return nil, err
		}
		p.Settings = string(raw)
		out = append(out, p)
	}
	return out, nil
}
