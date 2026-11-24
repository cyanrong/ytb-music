// Package localmusic indexes local audiophile music folders, parses lossless
// and DSD tags, CUE sheets, covers and LRC lyrics, and provides pure-Go
// decoders for playback.
package localmusic

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"ytb-music/internal/ytm"
)

const (
	TrackPrefix  = "localfile:"
	AlbumPrefix  = "localalbum:"
	ArtistPrefix = "localartist:"
	CoverScheme  = "localcover://"
)

// IsLocalTrack reports whether id is a local track ID.
func IsLocalTrack(id string) bool { return strings.HasPrefix(id, TrackPrefix) }

// IsLocalAlbum reports whether id is a local album ID.
func IsLocalAlbum(id string) bool { return strings.HasPrefix(id, AlbumPrefix) }

// IsLocalArtist reports whether id is a local artist ID.
func IsLocalArtist(id string) bool { return strings.HasPrefix(id, ArtistPrefix) }

// IsLocalCover reports whether u uses the localcover:// scheme.
func IsLocalCover(u string) bool { return strings.HasPrefix(u, CoverScheme) }

// CoverURL formats a cover ID as a localcover:// URL, or returns "" when empty.
func CoverURL(coverID string) string {
	if coverID == "" {
		return ""
	}
	if strings.HasPrefix(coverID, CoverScheme) {
		return coverID
	}
	return CoverScheme + coverID
}

// AudioSpec describes the codec, resolution, bitrate and CUE slice bounds of a
// local track or album.
type AudioSpec struct {
	Codec           string `json:"codec"`
	SampleRate      int    `json:"sampleRate"`
	BitDepth        int    `json:"bitDepth"`
	Channels        int    `json:"channels"`
	BitrateKbps     int    `json:"bitrateKbps"`
	IsLossless      bool   `json:"isLossless"`
	IsHiRes         bool   `json:"isHiRes"`
	IsDSD           bool   `json:"isDsd"`
	IsCUE           bool   `json:"isCue,omitempty"`
	ClipStartMs     int64  `json:"clipStartMs,omitempty"`
	ClipEndMs       int64  `json:"clipEndMs,omitempty"`
	ClipStartFrames int64  `json:"clipStartFrames,omitempty"`
	ClipEndFrames   int64  `json:"clipEndFrames,omitempty"`
	HasCueFrames    bool   `json:"hasCueFrames,omitempty"`
}

func (s *AudioSpec) computeFlags() {
	if s.IsDSD || strings.HasPrefix(strings.ToUpper(s.Codec), "DSD") {
		s.IsDSD = true
		s.IsLossless = true
		s.IsHiRes = true
		if s.BitDepth <= 0 {
			s.BitDepth = 1
		}
		return
	}
	switch strings.ToUpper(s.Codec) {
	case "FLAC", "WAV", "AIFF", "ALAC":
		s.IsLossless = true
	}
	s.IsHiRes = s.IsLossless && (s.SampleRate > 48000 || s.BitDepth > 16)
}

func formatKHz(hz int) string {
	if hz <= 0 {
		return ""
	}
	if hz%1000 == 0 {
		return strconv.Itoa(hz / 1000)
	}
	s := strconv.FormatFloat(float64(hz)/1000.0, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}

// Badge returns a concise quality badge such as "DSD128", "FLAC 24/96",
// "FLAC 16/44.1", or "MP3 320k", appending " · CUE" for CUE sheet tracks.
func (s AudioSpec) Badge() string {
	var base string
	switch {
	case s.IsDSD || strings.HasPrefix(strings.ToUpper(s.Codec), "DSD"):
		base = s.Codec
		if base == "" {
			base = dsdCodecName(s.SampleRate)
		}
	case s.IsLossless:
		if s.BitDepth > 0 && s.SampleRate > 0 {
			base = fmt.Sprintf("%s %d/%s", s.Codec, s.BitDepth, formatKHz(s.SampleRate))
		} else if s.SampleRate > 0 {
			base = fmt.Sprintf("%s %skHz", s.Codec, formatKHz(s.SampleRate))
		} else {
			base = s.Codec
		}
	default:
		if s.Codec != "" && s.BitrateKbps > 0 {
			base = fmt.Sprintf("%s %dk", s.Codec, s.BitrateKbps)
		} else if s.Codec != "" && s.SampleRate > 0 {
			base = fmt.Sprintf("%s %skHz", s.Codec, formatKHz(s.SampleRate))
		} else {
			base = s.Codec
		}
	}
	if s.IsCUE {
		if base == "" {
			return "CUE"
		}
		return base + " · CUE"
	}
	return base
}

// Detail returns a detailed technical readout with sample rate, bit depth, and
// bitrate, such as "FLAC · 96kHz / 24bit · 2845 kbps" or "DSD128 · 5.64MHz / 1bit".
func (s AudioSpec) Detail() string {
	var parts []string
	if s.IsDSD || strings.HasPrefix(strings.ToUpper(s.Codec), "DSD") {
		codec := s.Codec
		if codec == "" {
			codec = dsdCodecName(s.SampleRate)
		}
		parts = append(parts, codec)
		bits := s.BitDepth
		if bits <= 0 {
			bits = 1
		}
		if s.SampleRate > 0 {
			mhz := strconv.FormatFloat(float64(s.SampleRate)/1e6, 'f', 2, 64)
			parts = append(parts, fmt.Sprintf("%sMHz / %dbit", mhz, bits))
		} else {
			parts = append(parts, fmt.Sprintf("%dbit", bits))
		}
		if s.IsCUE {
			parts = append(parts, "CUE")
		}
		return strings.Join(parts, " · ")
	}

	if s.Codec != "" {
		parts = append(parts, s.Codec)
	}
	if s.SampleRate > 0 && s.BitDepth > 0 {
		parts = append(parts, fmt.Sprintf("%skHz / %dbit", formatKHz(s.SampleRate), s.BitDepth))
	} else if s.SampleRate > 0 {
		parts = append(parts, fmt.Sprintf("%skHz", formatKHz(s.SampleRate)))
	}
	if s.BitrateKbps > 0 {
		parts = append(parts, fmt.Sprintf("%d kbps", s.BitrateKbps))
	}
	if s.IsCUE {
		parts = append(parts, "CUE")
	}
	return strings.Join(parts, " · ")
}

func dsdCodecName(sampleRate int) string {
	switch {
	case sampleRate >= 45158400:
		return "DSD1024"
	case sampleRate >= 22579200:
		return "DSD512"
	case sampleRate >= 11289600:
		return "DSD256"
	case sampleRate >= 5644800:
		return "DSD128"
	default:
		return "DSD64"
	}
}

func specQualityRank(s AudioSpec) int {
	if s.IsDSD {
		return 10_000_000 + s.SampleRate
	}
	if s.IsLossless {
		base := 1_000_000
		if s.IsHiRes {
			base = 5_000_000
		}
		bits := max(s.BitDepth, 16)
		return base + s.SampleRate*(bits/8)
	}
	return s.BitrateKbps
}

// LocalTrack is an indexed local audio file or a single track slice from a CUE sheet.
type LocalTrack struct {
	ID             string    `json:"id"`
	Path           string    `json:"path"`
	Dir            string    `json:"dir"`
	CuePath        string    `json:"cuePath,omitempty"`
	Title          string    `json:"title"`
	Artists        []string  `json:"artists,omitempty"`
	Album          string    `json:"album,omitempty"`
	AlbumArtist    string    `json:"albumArtist,omitempty"`
	AlbumID        string    `json:"albumId,omitempty"`
	ArtistIDs      []string  `json:"artistIds,omitempty"`
	Genres         []string  `json:"genres,omitempty"`
	Composer       string    `json:"composer,omitempty"`
	Year           int       `json:"year,omitempty"`
	TrackNo        int       `json:"trackNo,omitempty"`
	DiscNo         int       `json:"discNo,omitempty"`
	DurationMs     int64     `json:"durationMs"`
	CoverID        string    `json:"coverId,omitempty"`
	Spec           AudioSpec `json:"spec"`
	GainDB         float64   `json:"gainDb,omitempty"`
	AddedAt        int64     `json:"addedAt"`
	Size           int64     `json:"size"`
	ModTimeNs      int64     `json:"modTimeNs"`
	EmbeddedLyrics string    `json:"embeddedLyrics,omitempty"`
}

// ToYTMTrack converts t into a ytm.Track compatible with the player and views.
func (t *LocalTrack) ToYTMTrack() ytm.Track {
	if t == nil {
		return ytm.Track{}
	}
	var artists []ytm.Artist
	if len(t.Artists) > 0 {
		artists = make([]ytm.Artist, len(t.Artists))
		for i, name := range t.Artists {
			var aid string
			if i < len(t.ArtistIDs) {
				aid = t.ArtistIDs[i]
			} else if name != "" {
				aid = makeArtistID(name)
			}
			artists[i] = ytm.Artist{ID: aid, Name: name}
		}
	} else if t.AlbumArtist != "" {
		artists = []ytm.Artist{{ID: makeArtistID(t.AlbumArtist), Name: t.AlbumArtist}}
	}
	return ytm.Track{
		ID:       t.ID,
		Name:     t.Title,
		Artists:  artists,
		Album:    ytm.Album{ID: t.AlbumID, Name: t.Album, PicURL: CoverURL(t.CoverID)},
		Duration: t.DurationMs,
		Reason:   t.Spec.Badge(),
	}
}

// ToYTMStream builds a ytm.Stream carrying the track's audio specification,
// bitrate, and ReplayGain for real-time player bar and Signal Path telemetry.
func (t *LocalTrack) ToYTMStream() *ytm.Stream {
	if t == nil {
		return nil
	}
	lvl := "exhigh"
	if t.Spec.IsLossless || t.Spec.IsDSD {
		lvl = "lossless"
	}
	return &ytm.Stream{
		Size:       t.Size,
		Type:       strings.TrimPrefix(strings.ToLower(filepath.Ext(t.Path)), "."),
		Level:      lvl,
		Gain:       t.GainDB,
		Codec:      t.Spec.Codec,
		SampleRate: t.Spec.SampleRate,
		BitDepth:   t.Spec.BitDepth,
		Bitrate:    t.Spec.BitrateKbps,
		IsDSD:      t.Spec.IsDSD,
		IsCUE:      t.Spec.IsCUE,
	}
}

// NativeHQP reports whether HQPlayer can play this file directly by URI
// without PCM transcoding or CUE slicing.
func (t *LocalTrack) NativeHQP() bool {
	if t == nil || t.Spec.IsCUE || t.Spec.HasCueFrames || t.Spec.ClipStartMs != 0 || t.Spec.ClipEndMs != 0 || t.Spec.ClipStartFrames != 0 || t.Spec.ClipEndFrames != 0 {
		return false
	}
	switch strings.ToLower(filepath.Ext(t.Path)) {
	case ".flac", ".wav", ".aif", ".aiff", ".dsf", ".dff", ".wv", ".mp3", ".ogg":
		return true
	default:
		return false
	}
}

// LocalAlbum groups tracks belonging to a single album.
type LocalAlbum struct {
	ID          string       `json:"id"`
	Title       string       `json:"title"`
	AlbumArtist string       `json:"albumArtist,omitempty"`
	ArtistID    string       `json:"artistId,omitempty"`
	Year        int          `json:"year,omitempty"`
	Genres      []string     `json:"genres,omitempty"`
	CoverID     string       `json:"coverId,omitempty"`
	Dir         string       `json:"dir,omitempty"`
	Spec        AudioSpec    `json:"spec"`
	Tracks      []LocalTrack `json:"tracks,omitempty"`
	AddedAt     int64        `json:"addedAt"`
	DiscCount   int          `json:"discCount"`
}

// ToAlbumProfile converts a into a ytm.AlbumProfile for album detail & search views.
func (a *LocalAlbum) ToAlbumProfile() ytm.AlbumProfile {
	if a == nil {
		return ytm.AlbumProfile{}
	}
	var artists []ytm.Artist
	if a.AlbumArtist != "" {
		aid := a.ArtistID
		if aid == "" {
			aid = makeArtistID(a.AlbumArtist)
		}
		artists = []ytm.Artist{{ID: aid, Name: a.AlbumArtist}}
	}
	var pubTime int64
	if a.Year > 0 {
		pubTime = time.Date(a.Year, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	}
	var descParts []string
	if d := a.Spec.Detail(); d != "" {
		descParts = append(descParts, d)
	}
	if len(a.Genres) > 0 {
		descParts = append(descParts, strings.Join(a.Genres, " / "))
	}
	songs := make([]ytm.Track, len(a.Tracks))
	for i := range a.Tracks {
		songs[i] = a.Tracks[i].ToYTMTrack()
	}
	return ytm.AlbumProfile{
		Album:       ytm.Album{ID: a.ID, Name: a.Title, PicURL: CoverURL(a.CoverID)},
		Artists:     artists,
		PublishTime: pubTime,
		Size:        len(a.Tracks),
		Description: strings.Join(descParts, " · "),
		Kind:        a.Spec.Badge(),
		Songs:       songs,
	}
}

// LocalArtist summarizes an artist across local albums and tracks.
type LocalArtist struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	CoverID    string       `json:"coverId,omitempty"`
	AlbumCount int          `json:"albumCount"`
	TrackCount int          `json:"trackCount"`
	Genres     []string     `json:"genres,omitempty"`
	Albums     []LocalAlbum `json:"albums,omitempty"`
	Tracks     []LocalTrack `json:"tracks,omitempty"`
}

// ToArtistProfile converts ar into a ytm.ArtistProfile.
func (ar *LocalArtist) ToArtistProfile() ytm.ArtistProfile {
	if ar == nil {
		return ytm.ArtistProfile{}
	}
	songs := make([]ytm.Track, len(ar.Tracks))
	for i := range ar.Tracks {
		songs[i] = ar.Tracks[i].ToYTMTrack()
	}
	cover := CoverURL(ar.CoverID)
	trackCount := ar.TrackCount
	if trackCount == 0 {
		trackCount = len(ar.Tracks)
	}
	albumCount := ar.AlbumCount
	if albumCount == 0 {
		albumCount = len(ar.Albums)
	}
	return ytm.ArtistProfile{
		Artist:    ytm.Artist{ID: ar.ID, Name: ar.Name},
		Avatar:    cover,
		PicURL:    cover,
		BriefDesc: strings.Join(ar.Genres, " / "),
		MusicSize: trackCount,
		AlbumSize: albumCount,
		Songs:     songs,
	}
}

// GenreSummary summarizes a genre in the local library.
type GenreSummary struct {
	Name       string   `json:"name"`
	AlbumCount int      `json:"albumCount"`
	TrackCount int      `json:"trackCount"`
	Covers     []string `json:"covers,omitempty"`
}

// FolderEntry describes a subdirectory in folder-tree browsing.
type FolderEntry struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	SubdirCount int    `json:"subdirCount"`
	TrackCount  int    `json:"trackCount"`
	CoverID     string `json:"coverId,omitempty"`
}

// FolderNode represents a browsed directory with its immediate subdirectories and tracks.
type FolderNode struct {
	Path    string        `json:"path"`
	Name    string        `json:"name"`
	Parent  string        `json:"parent"`
	Subdirs []FolderEntry `json:"subdirs,omitempty"`
	Tracks  []LocalTrack  `json:"tracks,omitempty"`
}

// AlbumFilter controls filtering and sorting when querying local albums.
type AlbumFilter struct {
	Query string
	Genre string
	Focus string // "", "hires", "dsd", "lossless", "cue"
	Sort  string // "recent", "title", "artist", "year", "spec"
}

// RoonAlbumGenre carries genre tags fetched from Roon for an album.
type RoonAlbumGenre struct {
	Album  string
	Artist string
	Genres []string
}

// Stats summarizes library counts.
type Stats struct {
	Folders     int `json:"folders"`
	Albums      int `json:"albums"`
	Tracks      int `json:"tracks"`
	Artists     int `json:"artists"`
	Genres      int `json:"genres"`
	HiResAlbums int `json:"hiResAlbums"`
	DSDAlbums   int `json:"dsdAlbums"`
}

// ScanProgress reports incremental folder scan status.
type ScanProgress struct {
	Scanning bool   `json:"scanning"`
	Scanned  int    `json:"scanned"`
	Total    int    `json:"total"`
	Current  string `json:"current"`
}

func canonicalPathKey(p string) string {
	clean := filepath.Clean(p)
	if runtime.GOOS == "windows" {
		return strings.ToLower(clean)
	}
	return clean
}

func shortHash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:12])
}

func makeTrackID(path string, isCUE bool, trackNo int, clipStartMs int64) string {
	key := canonicalPathKey(path)
	if isCUE {
		key = fmt.Sprintf("%s#cue:%d:%d", key, trackNo, clipStartMs)
	}
	return TrackPrefix + shortHash(key)
}

func makeAlbumID(albumTitle, albumArtist, dir string) string {
	nt := normMatchKey(albumTitle)
	na := normMatchKey(albumArtist)
	var key string
	if nt != "" && na != "" && na != "variousartists" {
		key = nt + "|" + na
	} else if nt != "" {
		key = nt + "|dir:" + canonicalPathKey(dir)
	} else {
		key = "dir:" + canonicalPathKey(dir)
	}
	return AlbumPrefix + shortHash(key)
}

func makeArtistID(artistName string) string {
	na := normMatchKey(artistName)
	if na == "" {
		na = strings.ToLower(strings.TrimSpace(artistName))
	}
	return ArtistPrefix + shortHash(na)
}
