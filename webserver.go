package archive

import (
	"compress/gzip"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/golang/gddo/httputil/header"
	"github.com/h2non/filetype"
	"gopkg.in/kothar/brotli-go.v0/enc"
)

// CompressWebserverFiles recursively zips common webserver files in a given directory structure
func CompressWebserverFiles(dir string) ([]string, error) {
	return BrotliAndGzipFiles(dir, regexp.MustCompile(strings.Join(
		[]string{"js", "css", "html", "json", "svg", "ico", "eot", "otf", "ttf", "woff"}, "$|")+"$",
	))
}

// BrotliAndGzipFiles recursively compresses of all regex matched files in a given directory structure
func BrotliAndGzipFiles(dir string, rgx *regexp.Regexp) ([]string, error) {
	dir = filepath.Clean(dir)
	fileInfo, err := os.Stat(dir)
	if err != nil {
		return nil, err
	} else if !fileInfo.IsDir() {
		return nil, ErrPathIsNotDirectory
	}
	var (
		matches = []string{}
		gzw     *gzip.Writer
		brw     *enc.BrotliWriter
	)
	err = filepath.Walk(dir, func(p string, fileInfo os.FileInfo, err error) error {
		if err == nil && !fileInfo.IsDir() && rgx.FindString(fileInfo.Name()) != "" {
			inputFile, err := os.Open(p)
			if err != nil {
				return err
			}
			defer inputFile.Close()
			matches = append(matches, p)
			if filepath.Ext(p) != ".gz" && filepath.Ext(p) != ".br" {
				gzOut, err := os.Create(p + ".gz")
				if err != nil {
					return err
				}
				defer gzOut.Close()
				gzw, err = gzip.NewWriterLevel(gzOut, gzip.BestCompression)
				if err != nil {
					return err
				}
				io.Copy(gzw, inputFile)
				gzw.Close()
				brOut, err := os.Create(p + ".br")
				if err != nil {
					return err
				}
				defer brOut.Close()
				brw = enc.NewBrotliWriter(nil, brOut)
				inputFile.Seek(0, 0)
				io.Copy(brw, inputFile)
				brw.Close()
			}
		}
		return nil
	})
	return matches, err
}

type fileHandler struct {
	mtx              *sync.RWMutex
	rootDir          http.Dir
	contentTypeCache map[string]string
}

// FileServer will search for and serve compressed files if they are available
func FileServer(root http.Dir) http.Handler {
	return &fileHandler{
		mtx:              &sync.RWMutex{},
		rootDir:          root,
		contentTypeCache: map[string]string{},
	}
}

func (f *fileHandler) getCachedContentType(p string) (string, error) {
	f.mtx.RLock()
	val, exists := f.contentTypeCache[p]
	f.mtx.RUnlock()
	if !exists {
		return val, ErrContentTypeNotFound
	}
	return val, nil
}

func (f *fileHandler) cacheContentType(p, contentType string) {
	f.mtx.Lock()
	f.contentTypeCache[p] = contentType
	f.mtx.Unlock()
	return
}

func (f *fileHandler) determineContentType(p string, file http.File) string {
	contentType, _ := f.getCachedContentType(p)
	if contentType != "" {
		return contentType
	}
	contentType = mime.TypeByExtension(filepath.Ext(p))
	if contentType != "" {
		f.cacheContentType(p, contentType)
		return contentType
	}
	typeMatch, _ := filetype.MatchFile(p)
	if typeMatch.MIME.Value != "" {
		f.cacheContentType(p, typeMatch.MIME.Value)
		return typeMatch.MIME.Value
	}
	var size int
	fileInfo, err := file.Stat()
	if err != nil && fileInfo.Size() < 512 {
		size = int(fileInfo.Size())
	} else {
		size = 512
	}
	bytes := make([]byte, size)
	file.Read(bytes)
	contentType = http.DetectContentType(bytes)
	f.cacheContentType(p, contentType)
	return contentType
}

var (
	encoders   = []string{"br", "gzip", ""}
	extensions = []string{".br", ".gz", ""}
)

func (f *fileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tryServingContent := func(enc, ext string) error {
		p := r.URL.Path
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		if p == "/" {
			p = "/index.html"
		}
		originalPath := p
		p = filepath.FromSlash(p + ext)
		p = strings.Replace(p, "\\", "/", -1) // for Windows
		file, err := f.rootDir.Open(p)
		if err != nil {
			return err
		}
		defer file.Close()
		fileInfo, err := file.Stat()
		if err != nil {
			return err
		}
		if fileInfo.IsDir() {
			return ErrPathIsDirectory
		}
		w.Header().Set("Content-Encoding", enc)
		w.Header().Set("Content-Type", f.determineContentType(originalPath, file))
		http.ServeContent(w, r, r.URL.Path, fileInfo.ModTime(), file)
		return nil
	}
	specs := header.ParseAccept(r.Header, "Accept-Encoding")
	for i := range encoders {
		if len(specs) == 0 {
			if tryServingContent(encoders[i], extensions[i]) == nil {
				return
			}
		}
		for _, spec := range specs {
			if spec.Value == encoders[i] && spec.Q > 0 || extensions[i] == "" {
				if tryServingContent(encoders[i], extensions[i]) == nil {
					return
				}
			}
		}
	}
	http.NotFound(w, r)
}
