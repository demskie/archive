package archive

import (
	"archive/tar"
	"compress/gzip"
	"encoding/csv"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"strings"
	"sync"
	"time"
)

// package level errors
var (
	ErrPathIsDirectory          = errors.New("path is a directory")
	ErrPathIsNotDirectory       = errors.New("path is not a directory")
	ErrArchiverHasBeenDestroyed = errors.New("archiver has been destroyed")
	ErrNothingToArchive         = errors.New("nothing to archive")
	ErrContentTypeNotFound      = errors.New("content type not found")
)

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
