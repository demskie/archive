# archive

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
