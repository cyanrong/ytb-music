package app

import (
	"encoding/json"
	"errors"
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
	pl.Editable = true
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

// saveLocalPlaylistsList atomically writes candidate to disk when a.localPath is set.
func (a *app) saveLocalPlaylistsList(candidate []ytm.Playlist) error {
	if a.localPath == "" {
		return nil
	}
	b, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.localPath), 0o700); err != nil {
		return err
	}
	tmp := a.localPath + ".part"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, a.localPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
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

func (a *app) createLocalPlaylist(name, desc, coverURL string, tracks []ytm.Track) (*ytm.Playlist, error) {
	pl := ytm.Playlist{
		ID:          newLocalPlaylistID(),
		Name:        name,
		Description: desc,
		CoverURL:    coverURL,
		Tracks:      slices.Clone(tracks),
	}
	normalizeLocalPlaylist(&pl)
	next := slices.Insert(slices.Clone(a.localPlaylists), 0, pl)
	if err := a.saveLocalPlaylistsList(next); err != nil {
		return nil, err
	}
	a.localPlaylists = next
	a.syncLocalPlaylists()
	return a.playlists[pl.ID], nil
}

func (a *app) renameLocalPlaylist(id, name string) error {
	i := slices.IndexFunc(a.localPlaylists, func(pl ytm.Playlist) bool { return pl.ID == id })
	if i < 0 {
		return errors.New("本地歌单不存在")
	}
	next := slices.Clone(a.localPlaylists)
	next[i].Name = name
	if err := a.saveLocalPlaylistsList(next); err != nil {
		return err
	}
	a.localPlaylists = next
	a.syncLocalPlaylists()
	return nil
}

func (a *app) deleteLocalPlaylist(id string) error {
	i := slices.IndexFunc(a.localPlaylists, func(pl ytm.Playlist) bool { return pl.ID == id })
	if i < 0 {
		return errors.New("本地歌单不存在")
	}
	next := slices.Delete(slices.Clone(a.localPlaylists), i, i+1)
	if err := a.saveLocalPlaylistsList(next); err != nil {
		return err
	}
	a.localPlaylists = next
	delete(a.playlists, id)
	a.syncLocalPlaylists()
	return nil
}

func (a *app) addTracksToLocalPlaylist(id string, tracks ...ytm.Track) (added int, duplicate bool, err error) {
	i := slices.IndexFunc(a.localPlaylists, func(pl ytm.Playlist) bool { return pl.ID == id })
	if i < 0 {
		return 0, false, errors.New("本地歌单不存在")
	}
	nextList := slices.Clone(a.localPlaylists)
	pl := &nextList[i]
	existing := make(map[string]bool, len(pl.Tracks))
	for _, t := range pl.Tracks {
		existing[t.ID] = true
	}
	nextTracks := slices.Clone(pl.Tracks)
	for _, tr := range tracks {
		if tr.ID == "" {
			continue
		}
		if existing[tr.ID] {
			duplicate = true
			continue
		}
		existing[tr.ID] = true
		nextTracks = append(nextTracks, tr)
		added++
	}
	if added == 0 {
		return 0, duplicate, nil
	}
	pl.Tracks = nextTracks
	normalizeLocalPlaylist(pl)
	if err := a.saveLocalPlaylistsList(nextList); err != nil {
		return 0, duplicate, err
	}
	a.localPlaylists = nextList
	a.syncLocalPlaylists()
	return added, duplicate, nil
}

func (a *app) removeTrackFromLocalPlaylist(id, trackID string) error {
	i := slices.IndexFunc(a.localPlaylists, func(pl ytm.Playlist) bool { return pl.ID == id })
	if i < 0 {
		return errors.New("本地歌单不存在")
	}
	before := a.localPlaylists[i].Tracks
	after := slices.DeleteFunc(slices.Clone(before), func(t ytm.Track) bool { return t.ID == trackID })
	if len(after) == len(before) {
		return nil
	}
	nextList := slices.Clone(a.localPlaylists)
	nextList[i].Tracks = after
	if nextList[i].CoverURL != "" && len(after) == 0 {
		nextList[i].CoverURL = ""
	}
	normalizeLocalPlaylist(&nextList[i])
	if err := a.saveLocalPlaylistsList(nextList); err != nil {
		return err
	}
	a.localPlaylists = nextList
	a.syncLocalPlaylists()
	return nil
}

func (a *app) moveTrackToTopLocalPlaylist(id, trackID string) error {
	i := slices.IndexFunc(a.localPlaylists, func(pl ytm.Playlist) bool { return pl.ID == id })
	if i < 0 {
		return errors.New("本地歌单不存在")
	}
	before := a.localPlaylists[i].Tracks
	idx := slices.IndexFunc(before, func(t ytm.Track) bool { return t.ID == trackID })
	if idx <= 0 {
		return nil
	}
	reordered := make([]ytm.Track, 0, len(before))
	reordered = append(reordered, before[idx])
	reordered = append(reordered, before[:idx]...)
	reordered = append(reordered, before[idx+1:]...)
	nextList := slices.Clone(a.localPlaylists)
	nextList[i].Tracks = reordered
	normalizeLocalPlaylist(&nextList[i])
	if err := a.saveLocalPlaylistsList(nextList); err != nil {
		return err
	}
	a.localPlaylists = nextList
	a.syncLocalPlaylists()
	return nil
}
