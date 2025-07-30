package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

type downloader struct {
	baseURL      *url.URL
	visitedURLs  map[string]bool
	visitedMutex sync.Mutex
	downloadDir  string
	maxDepth     int
	client       *http.Client
	wg           sync.WaitGroup
	semaphore    chan struct{}
}

func newDownloader(startURL string, downloadDir string, maxDepth int, maxConcurrent int) (*downloader, error) {
	parsedURL, err := url.Parse(startURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %v", err)
	}

	if parsedURL.Scheme == "" {
		parsedURL.Scheme = "http"
	}

	err = os.MkdirAll(downloadDir, 0755)
	if err != nil {
		return nil, fmt.Errorf("failed to create download directory: %v", err)
	}

	return &downloader{
		baseURL:     parsedURL,
		visitedURLs: make(map[string]bool),
		downloadDir: downloadDir,
		maxDepth:    maxDepth,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		semaphore: make(chan struct{}, maxConcurrent),
	}, nil
}

func (d *downloader) Download() error {
	return d.downloadURL(d.baseURL.String(), 0)
}

func (d *downloader) downloadURL(rawURL string, depth int) error {
	if depth > d.maxDepth {
		return nil
	}

	// Проверяем и добавляем URL в список посещенных
	d.visitedMutex.Lock()
	if d.visitedURLs[rawURL] {
		d.visitedMutex.Unlock()
		return nil
	}
	d.visitedURLs[rawURL] = true
	d.visitedMutex.Unlock()

	// Обрабатываем URL
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %v", rawURL, err)
	}

	// Пропускаем внешние ссылки
	if parsedURL.Host != d.baseURL.Host {
		return nil
	}

	d.semaphore <- struct{}{}
	d.wg.Add(1)
	go func() {
		defer func() {
			<-d.semaphore
			d.wg.Done()
		}()

		log.Printf("Downloading: %s (depth %d)", rawURL, depth)

		resp, err := d.client.Get(rawURL)
		if err != nil {
			log.Printf("Failed to download %q: %v", rawURL, err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("Non-OK status for %q: %d", rawURL, resp.StatusCode)
			return
		}

		// Определяем путь для сохранения
		savePath := d.getSavePath(parsedURL)
		if err := os.MkdirAll(filepath.Dir(savePath), 0755); err != nil {
			log.Printf("Failed to create directory for %q: %v", savePath, err)
			return
		}

		// Читаем содержимое
		content, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("Failed to read response body for %q: %v", rawURL, err)
			return
		}

		// Сохраняем файл
		if err := os.WriteFile(savePath, content, 0644); err != nil {
			log.Printf("Failed to save %q: %v", savePath, err)
			return
		}

		// Если это HTML, парсим ссылки
		if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
			d.processHTML(content, parsedURL, depth)
		}
	}()

	return nil
}

func (d *downloader) getSavePath(u *url.URL) string {
	// Удаляем начальный слэш
	path := strings.TrimPrefix(u.Path, "/")

	// Если путь заканчивается на /, добавляем index.html
	if path == "" || strings.HasSuffix(path, "/") {
		path = path + "index.html"
	}

	// Создаем полный путь
	fullPath := filepath.Join(d.downloadDir, u.Host, path)

	// Если нет расширения, добавляем .html
	if filepath.Ext(fullPath) == "" {
		fullPath += ".html"
	}

	return fullPath
}

func (d *downloader) processHTML(content []byte, baseURL *url.URL, depth int) {
	doc, err := html.Parse(bytes.NewReader(content))
	if err != nil {
		log.Printf("Failed to parse HTML: %v", err)
		return
	}

	var processNode func(*html.Node)
	processNode = func(n *html.Node) {
		if n.Type == html.ElementNode {
			var attrName string
			switch n.Data {
			case "a", "link":
				attrName = "href"
			case "img", "script":
				attrName = "src"
			case "iframe":
				attrName = "src"
			}

			if attrName != "" {
				for i, attr := range n.Attr {
					if attr.Key == attrName {
						// Пропускаем пустые ссылки и якоря
						if attr.Val == "" || strings.HasPrefix(attr.Val, "#") {
							continue
						}

						// Разрешаем относительные URL
						absoluteURL, err := baseURL.Parse(attr.Val)
						if err != nil {
							log.Printf("Failed to parse URL %q: %v", attr.Val, err)
							continue
						}

						// Нормализуем URL
						absoluteURL.Fragment = ""
						absoluteURL.RawQuery = ""

						// Заменяем ссылку на локальный путь
						localPath := d.getSavePath(absoluteURL)
						relPath, err := filepath.Rel(filepath.Dir(d.getSavePath(baseURL)), localPath)
						if err != nil {
							log.Printf("Failed to calculate relative path: %v", err)
							continue
						}

						n.Attr[i].Val = filepath.ToSlash(relPath)

						// Загружаем ресурс
						d.downloadURL(absoluteURL.String(), depth+1)
					}
				}
			}
		}

		for c := n.FirstChild; c != nil; c = c.NextSibling {
			processNode(c)
		}
	}

	processNode(doc)
}

func (d *downloader) Wait() {
	d.wg.Wait()
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: ./webmirror <URL> [depth] [download_dir]")
		os.Exit(1)
	}

	startURL := os.Args[1]
	depth := 1
	downloadDir := "downloads"

	if len(os.Args) > 2 {
		var err error
		depth, err = strconv.Atoi(os.Args[2])
		if err != nil {
			log.Fatalf("Invalid depth: %v", err)
		}
	}

	if len(os.Args) > 3 {
		downloadDir = os.Args[3]
	}

	downloader, err := newDownloader(startURL, downloadDir, depth, 10)
	if err != nil {
		log.Fatal(err)
	}

	if err := downloader.Download(); err != nil {
		log.Fatal(err)
	}

	downloader.Wait()
	log.Println("Download completed!")
}
