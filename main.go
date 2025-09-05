package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lrstanley/go-ytdlp"
)

//go:embed index.html
var content embed.FS

type URLRequest struct {
	URLs []string `json:"urls"`
}

type VideoInfo struct {
	Title     string `json:"title"`
	Extractor string `json:"extractor"`
	Thumbnail string `json:"thumbnail"`
}

type FileInfo struct {
	Title       string `json:"title"`
	Extractor   string `json:"extractor"`
	Thumbnail   string `json:"thumbnail"`
	DownloadUrl string `json:"downloadUrl"`
}

type SessionResponse struct {
	SessionID string `json:"sessionId"`
}

var sessions sync.Map

func isValidURL(url string) bool {
	return strings.HasPrefix(url, "https://www.youtube.com/") ||
		strings.HasPrefix(url, "https://youtu.be/") ||
		strings.HasPrefix(url, "https://soundcloud.com/") ||
		strings.HasPrefix(url, "https://on.soundcloud.com/")
}

func generateUniqueID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// converts titles to filesystem-safe strings
func safeFilename(title string) string {
	// had to do this for a specific video... was driving me nuts
	invalidChars := []string{"<", ">", ":", "\"", "/", "\\", "|", "?", "*", ";", "&", "`", "！", "：", "？"}
	for _, c := range invalidChars {
		title = strings.ReplaceAll(title, c, "_")
	}
	if len(title) > 200 {
		title = title[:200]
	}
	return title
}

// downloads audio and returns filesystem-safe filename, display name, thumbnail
func downloadAudio(url string) (string, string, string, error) {
	if !isValidURL(url) {
		return "", "", "", fmt.Errorf("invalid URL: must be YouTube or SoundCloud")
	}

	// fetch metadata
	infoCmd := ytdlp.New().DumpJSON()
	metaResult, err := infoCmd.Run(context.TODO(), url)
	if err != nil {
		return "", "", "", fmt.Errorf("metadata fetch error: %w", err)
	}

	var info VideoInfo
	if err := json.Unmarshal([]byte(metaResult.Stdout), &info); err != nil {
		return "", "", "", fmt.Errorf("metadata parse error: %w", err)
	}

	// filesystem-safe filename
	fsFilename := fmt.Sprintf("%s - %s.mp3", safeFilename(info.Extractor), safeFilename(info.Title))

	// download audio
	dl := ytdlp.New().
		ExtractAudio().
		AudioFormat("mp3").
		EmbedMetadata().
		EmbedThumbnail().
		Output(fsFilename)

	if _, err := dl.Run(context.TODO(), url); err != nil {
		return "", "", "", fmt.Errorf("download error: %w", err)
	}

	// verify file exists
	if _, err := os.Stat(fsFilename); os.IsNotExist(err) {
		return "", "", "", fmt.Errorf("output file not found: %s", fsFilename)
	}

	displayName := fmt.Sprintf("%s - %s.mp3", info.Extractor, info.Title)
	return fsFilename, displayName, info.Thumbnail, nil
}

// creates a ZIP archive of multiple files
func createZipFile(filenames []string) (string, error) {
	zipFilename := fmt.Sprintf("songs_%s.zip", generateUniqueID())
	zipFile, err := os.Create(zipFilename)
	if err != nil {
		return "", fmt.Errorf("error creating ZIP file: %w", err)
	}
	defer zipFile.Close()

	zipWriter := zip.NewWriter(zipFile)
	for _, filename := range filenames {
		file, err := os.Open(filename)
		if err != nil {
			return "", fmt.Errorf("error opening file for ZIP: %w", err)
		}
		defer file.Close()

		writer, err := zipWriter.Create(filepath.Base(filename))
		if err != nil {
			return "", fmt.Errorf("error adding file to ZIP: %w", err)
		}

		if _, err := io.Copy(writer, file); err != nil {
			return "", fmt.Errorf("error writing file to ZIP: %w", err)
		}
	}

	if err := zipWriter.Close(); err != nil {
		return "", fmt.Errorf("error closing ZIP writer: %w", err)
	}

	return zipFilename, nil
}

func processURLs(urls []string, ch chan string) {
	var filenames []string
	var fileInfos []FileInfo

	for _, url := range urls {
		log.Printf("Processing: %s", url)
		fsFilename, displayName, thumbnail, err := downloadAudio(url)
		if err != nil {
			log.Printf("[-] Download error for %s: %v", url, err)
			ch <- fmt.Sprintf("event: error\ndata: {\"url\":\"%s\",\"message\":\"%s\"}\n\n", url, err)
			continue
		}
		filenames = append(filenames, fsFilename)
		fileId := generateUniqueID()
		downloadUrl := fmt.Sprintf("/file/%s", fileId)

		http.HandleFunc(downloadUrl, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "http://localhost:4444")
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusOK)
				return
			}
			file, err := os.Open(fsFilename)
			if err != nil {
				http.Error(w, "error opening file", http.StatusInternalServerError)
				log.Printf("[-] Error opening file %s: %v", fsFilename, err)
				return
			}
			defer file.Close()
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", displayName))
			w.Header().Set("Content-Type", "audio/mpeg")
			if _, err := io.Copy(w, file); err != nil {
				log.Printf("[-] Error streaming file %s: %v", fsFilename, err)
			}
		})

		fileInfo := FileInfo{
			Title:       displayName,
			Extractor:   strings.Split(displayName, " - ")[0],
			Thumbnail:   thumbnail,
			DownloadUrl: downloadUrl,
		}
		fileInfos = append(fileInfos, fileInfo)

		fileJSON, err := json.Marshal(fileInfo)
		if err != nil {
			log.Printf("[-] Error marshaling file info: %v", err)
			continue
		}
		ch <- fmt.Sprintf("event: file\ndata: %s\n\n", fileJSON)
	}

	var zipUrl string
	if len(filenames) > 1 {
		zipFilename, err := createZipFile(filenames)
		if err != nil {
			log.Printf("[-] Error creating ZIP: %v", err)
		} else {
			zipUrl = fmt.Sprintf("/zip/%s", generateUniqueID())
			http.HandleFunc(zipUrl, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Access-Control-Allow-Origin", "http://localhost:4444")
				w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
				if r.Method == http.MethodOptions {
					w.WriteHeader(http.StatusOK)
					return
				}
				file, err := os.Open(zipFilename)
				if err != nil {
					http.Error(w, "error opening ZIP file", http.StatusInternalServerError)
					log.Printf("[-] Error opening ZIP %s: %v", zipFilename, err)
					return
				}
				defer file.Close()
				w.Header().Set("Content-Disposition", "attachment; filename=\"songs.zip\"")
				w.Header().Set("Content-Type", "application/zip")
				if _, err := io.Copy(w, file); err != nil {
					log.Printf("[-] Error streaming ZIP %s: %v", zipFilename, err)
				}
			})
			ch <- fmt.Sprintf("event: zip\ndata: \"%s\"\n\n", zipUrl)

			// cleanup ZIP file after 5 minutes
			go func(f string) {
				time.Sleep(5 * time.Minute)
				if err := os.Remove(f); err != nil {
					log.Printf("[-] Error cleaning up ZIP file %s: %v", f, err)
				}
			}(zipFilename)
		}
	}

	ch <- "event: done\ndata: {}\n\n"
	close(ch)

	// cleanup audio files after 5 minutes -> adjust as needed
	for _, f := range filenames {
		go func(f string) {
			time.Sleep(5 * time.Minute)
			if err := os.Remove(f); err != nil {
				log.Printf("[-] Error cleaning up file %s: %v", f, err)
			}
		}(f)
	}
}

func main() {
	ytdlp.MustInstall(context.TODO(), nil)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "http://localhost:4444")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		data, err := content.ReadFile("index.html")
		if err != nil {
			http.Error(w, "index.html not found", http.StatusInternalServerError)
			log.Printf("[-] Error reading index.html: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(data))
	})

	http.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "http://localhost:4444")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req URLRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.URLs) == 0 {
			http.Error(w, "invalid request: URLs required", http.StatusBadRequest)
			return
		}

		sessionID := generateUniqueID()
		ch := make(chan string)
		sessions.Store(sessionID, ch)

		go processURLs(req.URLs, ch)

		response := SessionResponse{SessionID: sessionID}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			log.Printf("[-] Error encoding response: %v", err)
		}
	})

	http.HandleFunc("/progress/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "http://localhost:4444")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		parts := strings.Split(r.URL.Path, "/")
		sessionID := parts[len(parts)-1]

		v, ok := sessions.Load(sessionID)
		if !ok {
			http.NotFound(w, r)
			return
		}
		ch := v.(chan string)

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		for msg := range ch {
			fmt.Fprint(w, msg)
			flusher.Flush()
		}

		sessions.Delete(sessionID)
	})

	log.Println("Server running at http://localhost:4444")
	log.Fatal(http.ListenAndServe("0.0.0.0:4444", nil))
}
