package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/grafov/m3u8"
)

var (
	playlistMap = make(map[string]string) // id -> remote URL
	segmentMap  = make(map[string]string) // id -> remote URL
	mapLock     = sync.RWMutex{}
	cacheDir    = "./cache"
	maxRetries  = 30
	retryDelay  = 500 * time.Millisecond
)

func main() {
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		panic(err)
	}

	r := gin.Default()

	r.GET("/master.m3u8", func(c *gin.Context) {
		url := c.Query("url")

		c.String(200, generatePlaylistWithLocalURIs(url, true))
	})

	r.GET("/:id/proxy.m3u8", func(c *gin.Context) {
		id := c.Param("id")

		mapLock.RLock()
		url, ok := playlistMap[id]
		mapLock.RUnlock()

		if !ok {
			c.String(404, "playlist not found")
			return
		}

		// This should serve the playlist rewritten with /segment/:id URIs
		c.String(200, generatePlaylistWithLocalURIs(url, false))
	})

	// Serve or fetch segments
	r.GET("/segment/:id", func(c *gin.Context) {
		id := c.Param("id")

		mapLock.RLock()
		url, ok := segmentMap[id]
		mapLock.RUnlock()

		if !ok {
			c.String(404, "segment not found")
			return
		}

		// Download if missing
		filename := filepath.Join(cacheDir, id)
		if _, err := os.Stat(filename); os.IsNotExist(err) {
			if err := downloadWithRetries(url, filename); err != nil {
				c.String(500, "failed to fetch segment: %s", err)
				return
			}
		}

		c.File(filename)
	})

	r.Run(":3144")
}

func downloadWithRetries(url, filename string) error {
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		err := downloadToFile(url, filename)
		if err == nil {
			return nil
		}

		lastErr = err
		fmt.Printf("Attempt %d failed: %s\n", attempt, err)

		time.Sleep(retryDelay)
	}

	return fmt.Errorf("all %d retries failed for %s: %w", maxRetries, url, lastErr)
}

// Downloads the given URL to a local file safely (via temp file)
func downloadToFile(url, filename string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}

	tmpfile := filename + ".tmp"
	f, err := os.Create(tmpfile)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}

	return os.Rename(tmpfile, filename)
}

func generatePlaylistWithLocalURIs(streamURL string, generate_master bool) string {
	res, err := http.Get(streamURL)
	if err != nil {
		fmt.Printf("error making http request: %s\n", err)
		return ""
	}
	p, listType, err := m3u8.DecodeFrom(bufio.NewReader(res.Body), true)
	if err != nil {
		fmt.Println(err)
		return ""
	}

	switch listType {
	case m3u8.MASTER:
		masterpl := p.(*m3u8.MasterPlaylist)
		fmt.Printf("Master playlist has %d variants\n", len(masterpl.Variants))

		if generate_master {
			h := sha256.New()
			h.Write([]byte(streamURL))
			id := base64.URLEncoding.EncodeToString(h.Sum(nil))

			mapLock.Lock()
			playlistMap[id] = streamURL
			mapLock.Unlock()

			cloned_master := m3u8.NewMasterPlaylist()
			for _, variant := range masterpl.Variants {
				variant.URI = "/" + id + "/proxy.m3u8"
				cloned_master.Variants = append(cloned_master.Variants, variant)
			}

			var buf bytes.Buffer
			cloned_master.Encode().WriteTo(&buf)

			return buf.String()
		}

		if len(masterpl.Variants) == 0 {
			fmt.Println("No variants found.")
			return ""
		}

		// Naively pick the first variant
		mediaURL := masterpl.Variants[0].URI
		fmt.Println("Selected media playlist:", mediaURL)

		// Optional: resolve relative URLs
		mediaFullURL := resolveRelative(streamURL, mediaURL)
		fmt.Println(mediaFullURL)

		// Now fetch the media playlist
		mediaRes, err := http.Get(mediaFullURL)
		if err != nil {
			fmt.Printf("error fetching media playlist: %s\n", err)
			return ""
		}
		defer mediaRes.Body.Close()

		mp, listType, err := m3u8.DecodeFrom(bufio.NewReader(mediaRes.Body), true)
		if err != nil {
			fmt.Printf("error decoding media playlist: %s\n", err)
			return ""
		}

		if listType != m3u8.MEDIA {
			fmt.Println("Expected media playlist, got something else")
			return ""
		}

		mediaPl := mp.(*m3u8.MediaPlaylist)
		fmt.Printf("Media playlist has %d segments\n", mediaPl.Count())

		n := uint(len(mediaPl.Segments))
		cloned, _ := m3u8.NewMediaPlaylist(n, n)
		cloned.Closed = true

		for _, segment := range mediaPl.Segments {
			if segment == nil {
				continue
			}

			fullUrl := resolveRelative(mediaFullURL, segment.URI)
			h := sha256.New()
			h.Write([]byte(fullUrl))
			id := base64.URLEncoding.EncodeToString(h.Sum(nil))
			fmt.Printf("Segment URI as %s: %s\n", id, fullUrl)

			mapLock.Lock()
			segmentMap[id] = fullUrl
			mapLock.Unlock()

			newSeg := *segment
			newSeg.URI = "/segment/" + id

			cloned.AppendSegment(&newSeg)
		}

		fmt.Printf("Segment list length: %d, mediaPl.Count(): %d\n", len(mediaPl.Segments), mediaPl.Count())

		var buf bytes.Buffer
		cloned.Encode().WriteTo(&buf)

		return buf.String()
	}

	return ""
}

func resolveRelative(base, rel string) string {
	baseURL, err := url.ParseRequestURI(base)
	if err != nil {
		return rel
	}
	relURL, err := url.Parse(rel)
	if err != nil {
		return baseURL.ResolveReference(&url.URL{Path: rel}).String()
	}
	if relURL.IsAbs() {
		return rel
	}
	return baseURL.ResolveReference(relURL).String()
}
