# archive

## Compress Static Webserver Files

```Go
func main() {
    fileList, err := archive.CompressWebserverFiles("./build/")
    if err != nil {
        fmt.Printf("unable to compress static webserver files because: %v\n", err)
    }
    fmt.Printf("compressed the following: %v\n", fileList)
}
```

## Serve Static Webserver Files

```Go
func main() {
    mux := http.NewServeMux()
    mux.Handle("/", archive.FileServer(http.Dir("./build/")))
    http.ListenAndServe(":80", mux)
}
```

## Create Gzipped CSV and Insert Into Tarball

```Go
func main() {
    csvData := [][]string{}
    csvData = append(csvData, []string{
        "hello world", "I'm a csv line",
    })
    archiver := archive.NewArchiver()
    archiver.AddCSV("csvData", csvData)
    archiver.CreateArchive("output")
    archiver.Destroy()
}
```
