package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo"

	"ytb-music/internal/ytm"
)

const localPlaylistPrefix = "local:"

var localIDSeq atomic.Int64

// isLocalPlaylist reports whether id names a playlist stored on disk rather
// than on YouTube Music.
func isLocalPlaylist(id string) bool {
	return strings.HasPrefix(id, localPlaylistPrefix)
}

var localPlaylistsPath = func() (string, error) {
	dir, err := mygo.App.Path(mygo.PathUserData)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "local_playlists.json"), nil
}

// initLocalPlaylists loads persistent local playlists from disk on startup.
func (a *app) initLocalPlaylists() {
	path, err := localPlaylistsPath()
	if err != nil {
		return
	}
	a.localPath = path
	b, err := os.ReadFile(path)
	if err == nil {
		var list []ytm.Playlist
		if json.Unmarshal(b, &list) == nil {
			for i := range list {
				if !isLocalPlaylist(list[i].ID) {
					list[i].ID = newLocalPlaylistID()
				}
				normalizeLocalPlaylist(&list[i])
			}
			a.localPlaylists = list
		}
	}
	a.syncLocalPlaylists()
}

func newLocalPlaylistID() string {
	n := localIDSeq.Add(1)
	return localPlaylistPrefix + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatInt(n, 36)
}

func normalizeLocalPlaylist(pl *ytm.Playlist) {
	pl.TrackCount = len(pl.Tracks)
	if pl.Creator.Nickname == "" {
		pl.Creator.Nickname = "本地歌单"
	}
	if pl.CoverURL == "" {
		for i := range pl.Tracks {
			if u := pl.Tracks[i].Album.PicURL; u != "" {
				pl.CoverURL = u
				break
			}
		}
	}
}

func sidebarLocalCopy(pl ytm.Playlist) ytm.Playlist {
	normalizeLocalPlaylist(&pl)
	pl.Tracks = nil
	pl.TrackIDs = nil
	return pl
}

// saveLocalPlaylists atomically persists local playlists when a.localPath is set.
func (a *app) saveLocalPlaylists() {
	if a.localPath == "" {
		return
	}
	b, err := json.MarshalIndent(a.localPlaylists, "", "  ")
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(a.localPath), 0o700) != nil {
		return
	}
	tmp := a.localPath + ".part"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, a.localPath)
	}
}

// withLocalPlaylists returns cloud playlists merged with the listener's local playlists.
func (a *app) withLocalPlaylists(cloud []ytm.Playlist) []ytm.Playlist {
	out := make([]ytm.Playlist, 0, len(cloud)+len(a.localPlaylists))
	for _, pl := range cloud {
		if !isLocalPlaylist(pl.ID) {
			out = append(out, pl)
		}
	}
	for _, pl := range a.localPlaylists {
		out = append(out, sidebarLocalCopy(pl))
	}
	return out
}

// syncLocalPlaylists refreshes a.playlists and a.mine from a.localPlaylists.
func (a *app) syncLocalPlaylists() {
	if a.playlists == nil {
		a.playlists = map[string]*ytm.Playlist{}
	}
	for i := range a.localPlaylists {
		normalizeLocalPlaylist(&a.localPlaylists[i])
		cp := a.localPlaylists[i]
		cp.Tracks = slices.Clone(cp.Tracks)
		a.playlists[cp.ID] = &cp
	}
	if len(a.localPlaylists) > 0 || slices.ContainsFunc(a.mine, func(pl ytm.Playlist) bool { return isLocalPlaylist(pl.ID) }) {
		a.mine = a.withLocalPlaylists(a.mine)
	}
}

func (a *app) createLocalPlaylist(name, desc, coverURL string, tracks []ytm.Track) *ytm.Playlist {
	pl := ytm.Playlist{
		ID:          newLocalPlaylistID(),
		Name:        name,
		Description: desc,
		CoverURL:    coverURL,
		Tracks:      slices.Clone(tracks),
	}
	normalizeLocalPlaylist(&pl)
	a.localPlaylists = slices.Insert(a.localPlaylists, 0, pl)
	a.syncLocalPlaylists()
	a.saveLocalPlaylists()
	return a.playlists[pl.ID]
}

func (a *app) renameLocalPlaylist(id, name string) bool {
	i := slices.IndexFunc(a.localPlaylists, func(pl ytm.Playlist) bool { return pl.ID == id })
	if i < 0 {
		return false
	}
	a.localPlaylists[i].Name = name
	a.syncLocalPlaylists()
	a.saveLocalPlaylists()
	return true
}

func (a *app) deleteLocalPlaylist(id string) bool {
	i := slices.IndexFunc(a.localPlaylists, func(pl ytm.Playlist) bool { return pl.ID == id })
	if i < 0 {
		return false
	}
	a.localPlaylists = slices.Delete(slices.Clone(a.localPlaylists), i, i+1)
	delete(a.playlists, id)
	a.syncLocalPlaylists()
	a.saveLocalPlaylists()
	return true
}

func (a *app) addTracksToLocalPlaylist(id string, tracks ...ytm.Track) (added int, duplicate bool) {
	i := slices.IndexFunc(a.localPlaylists, func(pl ytm.Playlist) bool { return pl.ID == id })
	if i < 0 {
		return 0, false
	}
	pl := &a.localPlaylists[i]
	existing := make(map[string]bool, len(pl.Tracks))
	for _, t := range pl.Tracks {
		existing[t.ID] = true
	}
	next := slices.Clone(pl.Tracks)
	for _, tr := range tracks {
		if tr.ID == "" {
			continue
		}
		if existing[tr.ID] {
			duplicate = true
			continue
		}
		existing[tr.ID] = true
		next = append(next, tr)
		added++
	}
	pl.Tracks = next
	normalizeLocalPlaylist(pl)
	a.syncLocalPlaylists()
	a.saveLocalPlaylists()
	return added, duplicate
}

func (a *app) removeTrackFromLocalPlaylist(id, trackID string) bool {
	i := slices.IndexFunc(a.localPlaylists, func(pl ytm.Playlist) bool { return pl.ID == id })
	if i < 0 {
		return false
	}
	before := a.localPlaylists[i].Tracks
	after := slices.DeleteFunc(slices.Clone(before), func(t ytm.Track) bool { return t.ID == trackID })
	if len(after) == len(before) {
		return false
	}
	a.localPlaylists[i].Tracks = after
	if a.localPlaylists[i].CoverURL != "" && len(after) == 0 {
		a.localPlaylists[i].CoverURL = ""
	}
	normalizeLocalPlaylist(&a.localPlaylists[i])
	a.syncLocalPlaylists()
	a.saveLocalPlaylists()
	return true
}
