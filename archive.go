package archive

import (
	"archive/tar"
	"compress/gzip"
	"encoding/csv"
	"errors"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/golang/gddo/httputil/header"
	"github.com/h2non/filetype"
	"gopkg.in/kothar/brotli-go.v0/enc"
)

// package level errors
var (
	ErrPathIsDirectory          = errors.New("path is a directory")
	ErrPathIsNotDirectory       = errors.New("path is not a directory")
	ErrArchiverHasBeenDestroyed = errors.New("archiver has been destroyed")
	ErrNothingToArchive         = errors.New("nothing to archive")
	ErrContentTypeNotFound      = errors.New("content type not found")
)

// CompressWebserverFiles recursively zips common webserver files in a given directory structure
func CompressWebserverFiles(dir string) ([]string, error) {
	return CompressFiles(dir, regexp.MustCompile(strings.Join(
		[]string{"js", "css", "html", "json", "svg", "ico", "eot", "otf", "ttf", "woff"}, "$|")+"$",
	))
}

// CompressFiles recursively zips of all regex matched files in a given directory structure
func CompressFiles(dir string, rgx *regexp.Regexp) ([]string, error) {
	fileInfo, err := os.Stat(filepath.Clean(dir))
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
		p = path.Clean(p)
		if p == "/" {
			p = "/index.html"
		}
		originalPath := p
		p = filepath.FromSlash(p + ext)
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

type tempFile struct {
	name   string
	object *os.File
}

// Archiver is used to create tar.gz archives
type Archiver struct {
	mtx         *sync.Mutex
	filelist    []tempFile
	destroyChan chan struct{}
}

// NewArchiver creates an archiver
func NewArchiver() *Archiver {
	archiver := &Archiver{
		mtx:         &sync.Mutex{},
		filelist:    make([]tempFile, 0),
		destroyChan: make(chan struct{}, 1),
	}
	return archiver
}

// Destroy closes destroyChan to signal the destruction of this Archiver
func (a *Archiver) Destroy() {
	a.mtx.Lock()
	select {
	case <-a.destroyChan:
	default:
		close(a.destroyChan)
	}
	a.mtx.Unlock()
}

func (a *Archiver) isDestroyed() bool {
	select {
	case <-a.destroyChan:
		return true
	default:
		return false
	}
}

func (a *Archiver) deleteFileWhenDestroyed(filename string) {
	<-a.destroyChan
	os.Remove(filename)
}

// AddCSV creates a temporary csv file to be archived when CreateArchive() is called
func (a *Archiver) AddCSV(filename string, lines [][]string) error {
	a.mtx.Lock()
	defer a.mtx.Unlock()
	// ensure that the archiver is still valid
	if a.isDestroyed() {
		return ErrArchiverHasBeenDestroyed
	}
	// create a temporary file
	file, err := ioutil.TempFile("", "go_archiver_")
	if err != nil {
		return err
	}
	go a.deleteFileWhenDestroyed(file.Name())
	// encode information into temporary csv file
	writer := csv.NewWriter(file)
	writer.WriteAll(lines)
	err = writer.Error()
	if err != nil {
		return err
	}
	// move cursor back to the beginning of the temporary file
	file.Seek(0, 0)
	// add temporary file into file list
	filename = strings.Split(filename, ".")[0] + ".csv"
	a.filelist = append(a.filelist, tempFile{
		name:   filename,
		object: file,
	})
	return nil
}

// CreateArchive moves all pending temporary files into a tar.gz
func (a *Archiver) CreateArchive(p string) error {
	a.mtx.Lock()
	defer a.mtx.Unlock()
	// no need to continue if there is nothing to archive
	if len(a.filelist) == 0 {
		return ErrNothingToArchive
	}
	// create an empty tar.gz file
	p = strings.Split(p, ".")[0] + ".tar.gz"
	outputFile, err := os.Create(p)
	if err != nil {
		return err
	}
	defer outputFile.Close()
	// create the gzip encoder
	gzw := gzip.NewWriter(outputFile)
	defer gzw.Close()
	// create the tar encoder
	trw := tar.NewWriter(gzw)
	defer trw.Close()
	// iterate through every temporary file
	for _, file := range a.filelist {
		// prepare file deletion in case of an early exit
		// note: this is safe to call more than once
		defer os.Remove(file.object.Name())
		// feed fileInfo into tar.WriteHeader()
		fileInfo, err := file.object.Stat()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(fileInfo, file.name)
		if err != nil {
			return err
		}
		// change the filename as the temporary filename is not valid
		header.Name = file.name
		header.ModTime = time.Now()
		header.AccessTime = time.Now()
		header.ChangeTime = time.Now()
		err = trw.WriteHeader(header)
		if err != nil {
			return err
		}
		// push all file data into the tar encoder
		_, err = io.Copy(trw, file.object)
		if err != nil {
			return err
		}
		// remove the object now that we are finished
		os.Remove(file.object.Name())
	}
	return nil
}
