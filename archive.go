package archive

import (
	"archive/tar"
	"compress/gzip"
	"encoding/csv"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/kothar/brotli-go.v0/enc"
)

// package level errors
var (
	ErrPathIsNotDirectory       = errors.New("path is not a directory")
	ErrArchiverHasBeenDestroyed = errors.New("archiver has been destroyed")
	ErrNothingToArchive         = errors.New("nothing to archive")
)

func CompressWebserverFiles(dir string) ([]string, error) {
	return CompressFiles(dir, regexp.MustCompile(strings.Join(
		[]string{"js", "css", "html", "json", "ico", "eot", "otf", "ttf", "wof"}, "$|")+"$",
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
	matchedFiles := []string{}
	err = filepath.Walk(dir, func(path string, fileInfo os.FileInfo, err error) error {
		if err == nil && !fileInfo.IsDir() && rgx.FindString(fileInfo.Name()) != "" {
			matchedFiles = append(matchedFiles, path)
			if !strings.HasSuffix(path, ".gz") {
				_, err = os.Stat(filepath.Join(path, ".gz"))
				if err != nil {
					return err
				}
				if os.IsNotExist(err) {
					inputFile, err := os.Open(path)
					if err != nil {
						return err
					}
					outputFile, err := os.Create(filepath.Join(path, ".gz"))
					if err != nil {
						return err
					}
					defer outputFile.Close()
					gzw, err := gzip.NewWriterLevel(outputFile, gzip.BestCompression)
					if err != nil {
						return err
					}
					defer gzw.Close()
					_, err = io.Copy(gzw, inputFile)
					if err != io.EOF {
						return err
					}
				}
			}
			if !strings.HasSuffix(path, ".br") {
				_, err = os.Stat(filepath.Join(path, ".br"))
				if err != nil {
					return err
				}
				if os.IsNotExist(err) {
					inputFile, err := os.Open(path)
					if err != nil {
						return err
					}
					outputFile, err := os.Create(filepath.Join(path, ".br"))
					if err != nil {
						return err
					}
					defer outputFile.Close()
					params := enc.NewBrotliParams()
					if regexp.MustCompile(".js$|.css$|.html$|.json$|.ico$").FindString(fileInfo.Name()) != "" {
						params.SetMode(enc.TEXT)
					} else if regexp.MustCompile(".eot$|.otf$|.ttf$|.woff$").FindString(fileInfo.Name()) != "" {
						params.SetMode(enc.FONT)
					}
					btw := enc.NewBrotliWriter(params, outputFile)
					defer btw.Close()
					_, err = io.Copy(btw, inputFile)
					if err != io.EOF {
						return err
					}
				}
			}
		}
		return nil
	})
	return matchedFiles, err
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
func (a *Archiver) CreateArchive(path string) error {
	a.mtx.Lock()
	defer a.mtx.Unlock()
	// no need to continue if there is nothing to archive
	if len(a.filelist) == 0 {
		return ErrNothingToArchive
	}
	// create an empty tar.gz file
	path = strings.Split(path, ".")[0] + ".tar.gz"
	outputFile, err := os.Create(path)
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
