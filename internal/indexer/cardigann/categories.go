package cardigann

import (
	"slices"
	"strings"
)

// Newznab standard categories (Prowlarr NewznabStandardCategory).
var newznabByName = map[string]int{
	"Console": 1000, "Console/NDS": 1010, "Console/PSP": 1020, "Console/Wii": 1030, "Console/XBox": 1040,
	"Console/XBox 360": 1050, "Console/Wiiware": 1060, "Console/XBox 360 DLC": 1070, "Console/PS3": 1080,
	"Console/Other": 1090, "Console/3DS": 1110, "Console/PS Vita": 1120, "Console/WiiU": 1130,
	"Console/XBox One": 1140, "Console/PS4": 1180,
	"Movies": 2000, "Movies/Foreign": 2010, "Movies/Other": 2020, "Movies/SD": 2030, "Movies/HD": 2040,
	"Movies/UHD": 2045, "Movies/BluRay": 2050, "Movies/3D": 2060, "Movies/DVD": 2070, "Movies/WEB-DL": 2080,
	"Movies/x265": 2090,
	"Audio":       3000, "Audio/MP3": 3010, "Audio/Video": 3020, "Audio/Audiobook": 3030, "Audio/Lossless": 3040,
	"Audio/Other": 3050, "Audio/Foreign": 3060,
	"PC": 4000, "PC/0day": 4010, "PC/ISO": 4020, "PC/Mac": 4030, "PC/Mobile-Other": 4040, "PC/Games": 4050,
	"PC/Mobile-iOS": 4060, "PC/Mobile-Android": 4070,
	"TV": 5000, "TV/WEB-DL": 5010, "TV/Foreign": 5020, "TV/SD": 5030, "TV/HD": 5040, "TV/UHD": 5045,
	"TV/Other": 5050, "TV/Sport": 5060, "TV/Anime": 5070, "TV/Documentary": 5080, "TV/x265": 5090,
	"XXX": 6000, "XXX/DVD": 6010, "XXX/WMV": 6020, "XXX/XviD": 6030, "XXX/x264": 6040, "XXX/UHD": 6045,
	"XXX/Pack": 6050, "XXX/ImageSet": 6060, "XXX/Other": 6070, "XXX/SD": 6080, "XXX/WEB-DL": 6090,
	"Books": 7000, "Books/Mags": 7010, "Books/EBook": 7020, "Books/Comics": 7030, "Books/Technical": 7040,
	"Books/Other": 7050, "Books/Foreign": 7060,
	"Other": 8000, "Other/Misc": 8010, "Other/Hashed": 8020,
}

type catMapping struct {
	tracker string
	desc    string
	newznab int
}

type categoryMap struct {
	mappings []catMapping
	defaults []string
}

func newCategoryMap(c Caps) *categoryMap {
	m := &categoryMap{}
	for _, kv := range c.Categories {
		if id, ok := newznabByName[kv.Value]; ok {
			m.mappings = append(m.mappings, catMapping{tracker: kv.Key, newznab: id})
		}
	}
	for _, cm := range c.CategoryMappings {
		id, ok := newznabByName[cm.Cat]
		if !ok {
			continue
		}
		m.mappings = append(m.mappings, catMapping{tracker: cm.ID, desc: cm.Desc, newznab: id})
		if cm.Default {
			m.defaults = append(m.defaults, cm.ID)
		}
	}
	return m
}

// wanted is the set of Newznab categories query selects: a parent category
// (2000) also selects its children (2010…2090).
func wanted(query []int) map[int]bool {
	want := map[int]bool{}
	for _, q := range query {
		want[q] = true
		if q%1000 == 0 && q < 100000 {
			for _, id := range newznabByName {
				if id/1000 == q/1000 {
					want[id] = true
				}
			}
		}
	}
	return want
}

// toTracker maps query (Newznab) categories to tracker categories (with
// the children of a parent category).
func (m *categoryMap) toTracker(query []int) []string {
	want := wanted(query)
	var out []string
	seen := map[string]bool{}
	for _, cm := range m.mappings {
		if want[cm.newznab] && !seen[cm.tracker] {
			seen[cm.tracker] = true
			out = append(out, cm.tracker)
		}
	}
	return out
}

// fromTracker maps a tracker category id to Newznab ids.
func (m *categoryMap) fromTracker(v string) []int {
	return m.lookup(v, func(cm catMapping) string { return cm.tracker })
}

// fromTrackerDesc maps a tracker category description to Newznab ids.
func (m *categoryMap) fromTrackerDesc(v string) []int {
	return m.lookup(v, func(cm catMapping) string { return cm.desc })
}

func (m *categoryMap) lookup(v string, key func(catMapping) string) []int {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	var out []int
	for _, cm := range m.mappings {
		if k := key(cm); k != "" && strings.EqualFold(k, v) && !slices.Contains(out, cm.newznab) {
			out = append(out, cm.newznab)
		}
	}
	return out
}
