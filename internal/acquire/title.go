package acquire

import "context"

// TitleInfo is a TMDB title for the «Найти и добавить» page of a title
// that is not in the library yet.
type TitleInfo struct {
	Candidate
	OriginalTitle string   `json:"originalTitle,omitempty"`
	Backdrop      string   `json:"backdrop"`
	Genres        []string `json:"genres"`
	Runtime       int      `json:"runtime,omitempty"` // minutes
	Rating        string   `json:"rating,omitempty"`
	// ImdbID ("tt0816692"): its ratings come from /api/ratings/{imdbId}.
	ImdbID string `json:"imdbId,omitempty"`
	// Cast: the leading actors (as a library item's cast).
	Cast []CastMember `json:"cast"`
	// Series: regular seasons (no specials) with their episode counts.
	Seasons []SeasonInfo `json:"seasons"`
	// Adult: TMDB marks it adult content (kept from kid profiles).
	Adult bool `json:"-"`
}

// CastMember is an actor and the role.
type CastMember struct {
	Name      string `json:"name"`
	Character string `json:"character,omitempty"`
	Photo     string `json:"photo,omitempty"`
}

// SeasonInfo is a season of a series on its title page.
type SeasonInfo struct {
	Number   int `json:"number"`
	Episodes int `json:"episodes"`
}

// Title returns TMDB details of a movie or series.
func (s *Service) Title(ctx context.Context, kind string, id int) (TitleInfo, error) {
	d, err := s.title(ctx, kind, id)
	if err != nil {
		return TitleInfo{}, err
	}
	m := s.o.Metadata
	t := TitleInfo{
		Candidate: Candidate{TMDBID: id, Kind: kind, Title: d.Title, Year: d.Year, Overview: d.Overview},
		Genres:    d.Genres, Runtime: d.Runtime, Rating: d.Rating, ImdbID: d.IMDbID, Adult: d.Adult,
	}
	if d.OriginalTitle != d.Title {
		t.OriginalTitle = d.OriginalTitle
	}
	if t.Genres == nil {
		t.Genres = []string{}
	}
	t.Cast = []CastMember{}
	for _, p := range d.Cast {
		t.Cast = append(t.Cast, CastMember{Name: p.Name, Character: p.Character, Photo: m.ImageURL("w185", p.Profile)})
	}
	t.Seasons = []SeasonInfo{}
	for _, se := range d.Seasons {
		if se.Number > 0 && se.Episodes > 0 {
			t.Seasons = append(t.Seasons, SeasonInfo{Number: se.Number, Episodes: se.Episodes})
		}
	}
	if d.Poster != "" {
		t.Poster = m.ImageURL("w342", d.Poster)
	}
	if d.Backdrop != "" {
		t.Backdrop = m.ImageURL("w1280", d.Backdrop)
	}
	if s.o.Library != nil {
		if it, err := s.o.Library.ItemByTMDB(ctx, kind, int64(id)); err == nil {
			iid := it.ID
			t.InLibrary, t.LibraryItemID = true, &iid
		}
	}
	return t, nil
}

// certifier is a Metadata that looks up an age rating alone
// (*tmdb.Client.Certification: no details, a request budget of its own).
type certifier interface {
	Certification(ctx context.Context, kind string, id int) (string, error)
}

// TitleRating is the age rating of a TMDB movie or series ("" = none):
// what a kid profile's search is filtered by. It asks for the rating alone
// when the metadata client can, else for the details.
func (s *Service) TitleRating(ctx context.Context, kind string, id int) (string, error) {
	if !isVideoKind(kind) || id <= 0 {
		return "", uerr(ErrInvalid, "Неизвестный фильм или сериал")
	}
	if c, ok := s.o.Metadata.(certifier); ok {
		tk := "movie"
		if kind == KindSeries {
			tk = "tv"
		}
		return c.Certification(ctx, tk, id)
	}
	d, err := s.title(ctx, kind, id)
	return d.Rating, err
}

// StreamStatus is what a viewer waiting for «Смотреть сейчас» sees: how
// far the torrent is, without paths or names.
type StreamStatus struct {
	Status   string `json:"status"`
	Mode     string `json:"mode"`
	Size     int64  `json:"size"`
	Done     int64  `json:"done"`
	DownRate int64  `json:"downRate"`
	Peers    int    `json:"peers"`
	Seeds    int    `json:"seeds"`
	// BufferHave of BufferWant bytes are downloaded where the player needs
	// data next (file >= 0 only; BufferWant 0 = unknown).
	BufferHave int64 `json:"bufferHave"`
	BufferWant int64 `json:"bufferWant"`
	// Stalled: the torrent's Status.Stalled (no_peers | no_metadata).
	Stalled string `json:"stalled,omitempty"`
}

// bufferer is implemented by engine handles that report stream buffering.
type bufferer interface {
	Buffer(fileIndex int) (have, want int64)
}

// TorrentStatus returns the progress of one torrent (any non-kid profile)
// and, for file >= 0, how far the buffer of that file is.
func (s *Service) TorrentStatus(ih string, file int) (StreamStatus, error) {
	h, ok := s.o.Engine.Get(ih)
	if !ok {
		return StreamStatus{}, uerr(ErrNotFound, "Раздача не найдена")
	}
	st := h.Status()
	out := StreamStatus{Status: st.Status, Mode: st.Mode, Size: st.Size, Done: st.Done, DownRate: st.DownRate, Peers: st.Peers, Seeds: st.Seeds, Stalled: st.Stalled}
	if b, ok := h.(bufferer); ok && file >= 0 {
		out.BufferHave, out.BufferWant = b.Buffer(file)
	}
	return out, nil
}

// SetStreamCacheBytes changes the «Смотреть сейчас» cache limit (settings);
// it applies from the next prune.
func (s *Service) SetStreamCacheBytes(n int64) {
	s.cacheBytes.Store(n)
}

func (s *Service) streamCacheBytes() int64 { return s.cacheBytes.Load() }
