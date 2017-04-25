package ssed

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	homedir "github.com/mitchellh/go-homedir"
	"github.com/schollz/archiver"
	"github.com/schollz/bol/utils"
	"github.com/schollz/lumber"
	"github.com/schollz/pwdhash"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/host"
)

// Generic functions

type config struct {
	Username       string `json:"username"`
	HashedPassword string `json:"hashed_password"`
	Method         string `json:"method"`
}

var pathToConfigFolder string
var pathToConfigFile string
var pathToCacheFolder string
var pathToLocalFolder string
var pathToRemoteFolder string
var homePath string
var logger *lumber.ConsoleLogger

// PathToTempFolder is the path to the temporary folder created by ssed
var PathToTempFolder string
var LocalFolder string
var RemoteFolder string

// DebugMode enables logging
func DebugMode() {
	logger.Level(0)
}

func init() {
	logger = lumber.NewConsoleLogger(lumber.DEBUG)
	logger.Level(2)
}

func createDirs() {
	logger.Debug("Creating directories for ssed")
	dir, _ := homedir.Dir()
	homePath = dir
	if !utils.Exists(path.Join(dir, ".config")) {
		os.MkdirAll(path.Join(dir, ".config"), 0755)
	}
	if !utils.Exists(path.Join(dir, ".config", "ssed")) {
		os.MkdirAll(path.Join(dir, ".config", "ssed"), 0755)
	}
	if !utils.Exists(path.Join(dir, ".cache")) {
		os.MkdirAll(path.Join(dir, ".cache"), 0755)
	}
	if !utils.Exists(path.Join(dir, ".cache", "ssed")) {
		os.MkdirAll(path.Join(dir, ".cache", "ssed"), 0755)
	}
	if !utils.Exists(path.Join(dir, ".cache", "ssed", "temp")) {
		os.MkdirAll(path.Join(dir, ".cache", "ssed", "temp"), 0755)
	}
	if !utils.Exists(path.Join(dir, ".cache", "ssed", "local")) {
		os.MkdirAll(path.Join(dir, ".cache", "ssed", "local"), 0755)
	}
	if !utils.Exists(path.Join(dir, ".cache", "ssed", "remote")) {
		os.MkdirAll(path.Join(dir, ".cache", "ssed", "remote"), 0755)
	}
	pathToConfigFile = path.Join(dir, ".config", "ssed", "config.json")
	pathToConfigFolder = path.Join(dir, ".config", "ssed")
	pathToCacheFolder = path.Join(dir, ".cache", "ssed")
	PathToTempFolder = path.Join(dir, ".cache", "ssed", "temp")
	pathToLocalFolder = path.Join(dir, ".cache", "ssed", "local")
	LocalFolder = pathToLocalFolder
	pathToRemoteFolder = path.Join(dir, ".cache", "ssed", "remote")
	RemoteFolder = pathToRemoteFolder
}

// Entry is the fundamental unit of an entry in any document
type Entry struct {
	Text              string `json:"text"`
	Timestamp         string `json:"timestamp"`
	ModifiedTimestamp string `json:"modified_timestamp"`
	Document          string `json:"document"`
	Entry             string `json:"entry"`
	datetime          time.Time
	uuid              string
}

type document struct {
	Name    string
	Entries []Entry
}

// Fs is the filesystem for ssed
type Fs struct {
	wg               sync.WaitGroup
	havePassword     bool
	parsed           bool
	shredding        bool
	successfulPull   bool
	pathToSourceRepo string
	pathToLocalRepo  string
	pathToRemoteRepo string
	username         string
	password         string
	method           string
	archiveName      string
	entries          map[string]Entry    // uuid -> entry
	entryNameToUUID  map[string]string   // entry name -> uuid
	ordering         map[string][]string // document -> list of entry uuids in order
}

// GetBlankEntries returns an empty slice of entries
func GetBlankEntries() []Entry {
	return []Entry{}
}

// EraseConfig erases the folder containing ssed configuration files
func EraseConfig() {
	utils.Shred(pathToConfigFile)
}

// EraseAll erases the folders for configuration and cache for ssed
func EraseAll() {
	createDirs()
	CleanUp()
	EraseConfig()
	os.RemoveAll(pathToCacheFolder)
	os.RemoveAll(pathToConfigFolder)
	files, _ := filepath.Glob(path.Join(pathToCacheFolder, "*"))
	for _, file := range files {
		fmt.Println(file)
	}
}

// CleanUp shreds all the temporary files
func CleanUp() {
	utils.Shred(path.Join(PathToTempFolder, "temp"))
}

// HasPinFile
func (ssed *Fs) HasPinFile() bool {
	return utils.Exists(path.Join(pathToConfigFolder, ssed.username+".key"))
}

// GetPasswordFromPin allows to use a pin
func (ssed *Fs) GetPasswordFromPin(pin string) (string, error) {
	if !utils.Exists(path.Join(pathToConfigFolder, ssed.username+".key")) {
		return "", errors.New("Key not set")
	}
	hashPin, err := HashPasswordSlow(pin)
	if err != nil {
		os.Remove(path.Join(pathToConfigFolder, ssed.username+".key"))
		return "", err
	}
	bPassword, err := utils.DecryptFromFile(hashPin, path.Join(pathToConfigFolder, ssed.username+".key"))
	if err != nil {
		os.Remove(path.Join(pathToConfigFolder, ssed.username+".key"))
		return "", err
	}
	return string(bPassword), nil
}

// SetPinFromPassword allows to use a pin
func (ssed *Fs) SetPinFromPassword(pin string) error {
	hashPin, err := HashPasswordSlow(pin)
	if err != nil {
		return err
	}
	return utils.EncryptToFile([]byte(ssed.password), hashPin, path.Join(pathToConfigFolder, ssed.username+".key"))
}

// HashPasswordSlow generates a bcrypt hash of the password using work factor 1048576.
func HashPasswordSlow(password string) (string, error) {
	hostinfo, e := host.Info()
	if e != nil {
		return "", e
	}
	cpuinfo, e := cpu.Info()
	if e != nil {
		return "", e
	}
	salt := hostinfo.OS + hostinfo.Hostname + cpuinfo[0].ModelName
	t := time.Now()
	var hashedPass string
	workFactor := 65536
	for {
		p, err := pwdhash.GenerateFromPassword([]byte(password), []byte(salt), workFactor, 64, "sha512")
		if err != nil {
			return "", err
		}
		hashedPass = string(p)
		if time.Since(t) > 500*time.Millisecond {
			break
		}
		workFactor = workFactor + 65536
		t = time.Now()
	}
	return hashedPass, nil
}

// ReturnMethod returns the current method being used
func (ssed *Fs) ReturnMethod() string {
	return ssed.method
}

// ReturnUser returns the current user being used
func (ssed *Fs) ReturnUser() string {
	return ssed.username
}

// SetMethod sets the server to obtain the synchronization
func (ssed *Fs) SetMethod(method string) error {
	if !strings.Contains(method, "http") && !strings.Contains(method, "ssh") {
		return errors.New("Incorrect method provided")
	}
	ssed.method = method
	b, _ := ioutil.ReadFile(pathToConfigFile)
	var configs []config
	json.Unmarshal(b, &configs)
	for i := range configs {
		if configs[i].Username == ssed.username {
			// check if password matches
			configs[i].Method = method
			break
		}
	}
	b, _ = json.MarshalIndent(configs, "", "  ")
	ioutil.WriteFile(pathToConfigFile, b, 0755)
	return nil
}

// Init initializes the repo
// If the username and the method are left blank it will automatically use first
// found in the config file
func (ssed *Fs) Init(username, method string) error {
	defer timeTrack(time.Now(), "Opening archive")
	createDirs()
	// Load configuration
	err := ssed.loadConfiguration(username, method)
	if err != nil {
		return err
	}

	// create nessecary folders if nessecary
	ssed.pathToLocalRepo = path.Join(pathToCacheFolder, "local", ssed.username)
	if !utils.Exists(ssed.pathToLocalRepo) {
		os.MkdirAll(ssed.pathToLocalRepo, 0755)
	}
	ssed.pathToRemoteRepo = path.Join(pathToCacheFolder, "remote", ssed.username)
	if !utils.Exists(ssed.pathToRemoteRepo) {
		os.MkdirAll(ssed.pathToRemoteRepo, 0755)
	}

	// download and decompress asynchronously
	ssed.wg = sync.WaitGroup{}
	ssed.wg.Add(1)
	go ssed.downloadAndDecompress()
	return nil
}

func (ssed *Fs) loadConfiguration(username, method string) error {
	var configs []config
	if !utils.Exists(pathToConfigFile) {
		if len(username) == 0 {
			return errors.New("Need to have username to intialize for first time")
		}
		// if len(method) > 0 && !strings.Contains(method, "http") && !strings.Contains(method, "ssh") {
		// 	return errors.New("Method must be http or ssh")
		// }
		// Configuration file doesn't exists, create it
		configs = []config{
			{
				Username: username,
				Method:   method,
			},
		}
	} else {
		// Configuration file already exists
		// If the user does not exists, add user as new default and continue
		b, _ := ioutil.ReadFile(pathToConfigFile)
		json.Unmarshal(b, &configs)
		foundConfig := -1
		for i := range configs {
			// uses the default (first entry)
			if username == "" {
				username = configs[i].Username
			}
			if configs[i].Username == username {
				// check if password matches
				method = configs[i].Method
				foundConfig = i
				break
			}
		}
		if foundConfig == -1 {
			// configuration is new, and is added to the front as it will be the new default
			// if len(method) > 0 && !strings.Contains(method, "http") && !strings.Contains(method, "ssh") {
			// 	return errors.New("Method must be http or ssh")
			// }
			configs = append([]config{{
				Username: username,
				Method:   method,
			}}, configs...)
		} else {
			// configuration is old, but will add to front to be new default
			currentConfig := configs[foundConfig]
			otherConfigs := append(configs[:foundConfig], configs[foundConfig+1:]...)
			configs = append([]config{currentConfig}, otherConfigs...)
		}
	}

	b, _ := json.MarshalIndent(configs, "", "  ")
	ioutil.WriteFile(pathToConfigFile, b, 0755)

	ssed.method = configs[0].Method
	ssed.username = configs[0].Username
	ssed.archiveName = ssed.username + ".tar.bz2"
	return nil
}

func (ssed *Fs) downloadAndDecompress() {
	err := ssed.download()
	if err == nil {
		ssed.successfulPull = true
	}
	// unpack local and remote archives
	ssed.decompress()
	// copy over files
	ssed.copyOverFiles()
	// unlock the to allow it to continue
	ssed.wg.Done()
}

func (ssed *Fs) doesMD5MatchServer() (bool, error) {

	if !strings.Contains(ssed.method, "http") {
		return false, errors.New("No server available")
	}

	defer timeTrack(time.Now(), "doesMD5MatchServer")
	req, err := http.NewRequest("GET", ssed.method+"/md5", nil)
	if err != nil {
		return false, err
	}
	req.SetBasicAuth(ssed.username, "") // no password needed

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	htmlData, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	currentMD5, err := utils.ComputeMd5(path.Join(pathToLocalFolder, ssed.archiveName))
	if err != nil {
		logger.Debug("Problem: %s", err.Error())
	}
	matchingMD5 := currentMD5 == string(htmlData)
	logger.Debug("server md5 '%s' = local md5 '%s' = %v", string(htmlData), currentMD5, matchingMD5)
	return matchingMD5, nil
}

func (ssed *Fs) download() error {
	// download repo
	if !strings.Contains(ssed.method, "http") {
		return errors.New("No server available")
	}

	matching, err := ssed.doesMD5MatchServer()
	if err != nil {
		return err
	}
	if matching {
		logger.Debug("Not downloading since MD5 matches")
		return nil
	}
	defer timeTrack(time.Now(), "download")
	req, err := http.NewRequest("GET", ssed.method+"/repo", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(ssed.username, "") // no password needed

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	outFile, err := os.Create(path.Join(pathToRemoteFolder, ssed.archiveName))
	if err != nil {
		return err
	}
	_, err = io.Copy(outFile, resp.Body)
	if err != nil {
		return err
	}
	outFile.Close()

	return nil
}

func (ssed *Fs) decompress() {

	// open remote repo
	if !utils.Exists(path.Join(pathToRemoteFolder, ssed.username)) {
		os.Mkdir(path.Join(pathToRemoteFolder, ssed.username), 0755)
	}
	if utils.Exists(path.Join(pathToRemoteFolder, ssed.archiveName)) {
		wd, _ := os.Getwd()
		os.Chdir(pathToRemoteFolder)
		os.RemoveAll(ssed.username)
		logger.Debug("Opening remote")
		archiver.TarBz2.Open(ssed.archiveName, ssed.username)
		os.Chdir(wd)
	}

	// open local repo
	sourceRepo := path.Join(pathToLocalFolder, ssed.archiveName)
	if utils.Exists(sourceRepo) && !utils.Exists(path.Join(pathToLocalFolder, ssed.username)) {
		os.Mkdir(path.Join(pathToLocalFolder, ssed.username), 0755)
		wd, _ := os.Getwd()
		os.Chdir(pathToLocalFolder)
		logger.Debug("Opening local")
		archiver.TarBz2.Open(ssed.archiveName, ssed.username)
		os.Chdir(wd)
	}
	logger.Debug("Download Finished")
}

func (ssed *Fs) copyOverFiles() {
	localFiles := make(map[string]bool)
	files, _ := filepath.Glob(path.Join(pathToLocalFolder, ssed.username, "*"))
	for _, file := range files {
		localFiles[filepath.Base(file)] = true
	}

	files, _ = filepath.Glob(path.Join(pathToRemoteFolder, ssed.username, "*"))
	for _, file := range files {
		if _, ok := localFiles[filepath.Base(file)]; !ok {
			// local doesn't have this remote file! copy it over
			utils.CopyFile(path.Join(pathToRemoteFolder, ssed.username, filepath.Base(file)), path.Join(pathToLocalFolder, ssed.username, filepath.Base(file)))
			logger.Debug("Copying over " + filepath.Base(file))
		}
	}

}

// func openAndDecrypt(filename string, password string) (string, error) {
// 	key := sha256.Sum256([]byte(password))
// 	content, err := ioutil.ReadFile(filename)
// 	contentData, err := hex.DecodeString(string(content))
// 	if err != nil {
// 		return "", err
// 	}
// 	decrypted, err := cryptopasta.Decrypt(contentData, &key)
// 	return string(decrypted), err
// }

// Open attempts to open a ssed repostiroy using the specified password
func (ssed *Fs) Open(password string) error {
	// only continue if the downloading is finished
	ssed.wg.Wait()
	logger.Debug("Finished waiting")

	// check password against one of the files (if they exist)
	files, _ := filepath.Glob(path.Join(pathToLocalFolder, ssed.username, "*"))
	if len(files) > 0 {
		logger.Debug("Testing against %s", files[0])
		_, err := utils.DecryptFromFile(password, files[0])
		if err != nil {
			return err
		}
	}
	ssed.password = password
	return nil
}

// Update make a new entry
// date can be empty, it will fill in the current date if so
func (ssed *Fs) Update(text, documentName, entryName, timestamp string) error {
	if len(entryName) == 0 {
		entryName = utils.RandStringBytesMaskImprSrc(10)
	}
	fileName := path.Join(ssed.pathToLocalRepo, utils.HashAndHex(text+entryName)+".json")
	if utils.Exists(fileName) {
		return nil
	}

	if len(timestamp) == 0 {
		timestamp = utils.GetCurrentDate()
	} else {
		timestamp = utils.ReFormatDate(timestamp)
	}

	modifiedTimestamp := timestamp
	if ssed.entryExists(entryName) {
		modifiedTimestamp = utils.GetCurrentDate()
	}

	e := Entry{
		Text:              text,
		Document:          documentName,
		Entry:             entryName,
		Timestamp:         timestamp,
		ModifiedTimestamp: modifiedTimestamp,
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}

	// key := sha256.Sum256([]byte(ssed.password))
	// encrypted, _ := cryptopasta.Encrypt(b, &key)
	//
	// err = ioutil.WriteFile(fileName, []byte(hex.EncodeToString(encrypted)), 0755)
	err = utils.EncryptToFile(b, ssed.password, fileName)

	ssed.parsed = false
	if err == nil {
		logger.Debug("Inserted new entry, %s as %s", e.Entry, fileName)
	} else {
		logger.Error("Could not insert entry, %s because error: %s", e.Entry, err.Error())
	}
	return err
}

// DeleteEntry will simply Update("ignore-entry",documentName,entryName,"")
func (ssed *Fs) DeleteEntry(documentName, entryName string) {
	ssed.Update("ignore entry", documentName, entryName, "")
	ssed.parsed = false
}

// DeleteDocument will simply Update("ignore document",documentName,entryName,"")
func (ssed *Fs) DeleteDocument(documentName string) {
	ssed.Update("ignore document", documentName, "", "")
	ssed.parsed = false
}

// Close closes the repo and pushes if it was succesful pulling
func (ssed *Fs) Close() error {
	var err error
	defer timeTrack(time.Now(), "Closing archive")
	defer os.Remove(path.Join(PathToTempFolder, "temp"))
	wd, _ := os.Getwd()
	os.Chdir(path.Join(pathToLocalFolder, ssed.username))
	filesFullPath, _ := filepath.Glob(path.Join(pathToLocalFolder, ssed.username, "*.json"))
	fileList := make([]string, len(filesFullPath))
	logger.Debug("archiving %d files", len(filesFullPath))
	for i, file := range filesFullPath {
		fileList[i] = filepath.Base(file)
	}
	for _, ff := range archiver.SupportedFormats {
		if !ff.Match(ssed.archiveName) {
			continue
		}
		ff.Make(path.Join("..", ssed.archiveName), fileList)
		break
	}
	os.Chdir(wd)

	matching, err := ssed.doesMD5MatchServer()
	if ssed.successfulPull && !matching {
		err = ssed.upload()
		if err != nil {
			err = errors.New("Cannot connect, local changes saved.")
		}
	} else {
		if !ssed.successfulPull {
			err = errors.New("No internet, changes will be uploaded next time.")
		} else {
			err = errors.New("No changes, not uploading.")

		}
	}
	// // shred the data files
	// if ssed.shredding {
	// 	files, _ := filepath.Glob(path.Join(PathToTempFolder, "*", "*"))
	// 	for _, file := range files {
	// 		err := utils.Shred(file)
	// 		if err == nil {
	// 			// logger.Debug("Shredded %s", file)
	// 		}
	// 	}
	// 	// shred the archive
	// 	files, _ = filepath.Glob(path.Join(PathToTempFolder, "*"))
	// 	for _, file := range files {
	// 		err := utils.Shred(file)
	// 		if err == nil {
	// 			// logger.Debug("Shredded %s", file)
	// 		}
	// 	}
	// }
	return err
}

func (ssed *Fs) delete() error {
	if strings.Contains(ssed.method, "http") {
		logger.Debug("Deleting %s on remote", ssed.username)

		req, err := http.NewRequest("DELETE", ssed.method+"/repo", nil)
		if err != nil {
			return err
		}
		req.SetBasicAuth(ssed.username, ssed.password)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		_, err = io.Copy(os.Stdout, resp.Body)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
	}
	return nil
}

func (ssed *Fs) upload() error {
	defer timeTrack(time.Now(), "Uploading archive")
	wd, _ := os.Getwd()

	if strings.Contains(ssed.method, "http") {
		os.Chdir(pathToLocalFolder)
		logger.Debug("Pushing")
		// Generated by curl-to-Go: https://mholt.github.io/curl-to-go
		file, err := os.Open(ssed.archiveName)
		defer file.Close()
		req, err := http.NewRequest("POST", ssed.method+"/repo", file)
		if err != nil {
			return err
		}
		req.SetBasicAuth(ssed.username, ssed.password)
		req.Header.Set("Content-Type", "application/octet-stream")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		_, err = io.Copy(os.Stdout, resp.Body)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		os.Chdir(wd)
	}
	return nil
}

type timeSlice []Entry

func (p timeSlice) Len() int {
	return len(p)
}

// need to sort in reverse-chronological order to get only newest
func (p timeSlice) Less(i, j int) bool {
	return p[j].datetime.Before(p[i].datetime)
}

func (p timeSlice) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
}

func (ssed *Fs) parseArchive() {
	defer timeTrack(time.Now(), "Parsing archive")
	files, _ := filepath.Glob(path.Join(ssed.pathToLocalRepo, "*"))
	ssed.entries = make(map[string]Entry)
	ssed.entryNameToUUID = make(map[string]string)
	ssed.ordering = make(map[string][]string)
	var entriesToSortByModified = make(map[string]Entry)
	for _, file := range files {
		logger.Debug("Parsing %s", file)
		// key := sha256.Sum256([]byte(ssed.password))
		// content, err := ioutil.ReadFile(file)
		// if err != nil {
		// 	panic(err)
		// }
		// contentData, err := hex.DecodeString(string(content))
		// if err != nil {
		// 	panic(err)
		// }
		decrypted, err := utils.DecryptFromFile(ssed.password, file)
		if err != nil {
			panic(err)
		}

		var e Entry
		err = json.Unmarshal(decrypted, &e)
		if err != nil {
			panic(err)
		}
		e.uuid = filepath.Base(file)
		e.datetime, _ = utils.ParseDate(e.Timestamp)
		if len(e.ModifiedTimestamp) > 0 {
			e.datetime, _ = utils.ParseDate(e.ModifiedTimestamp) // sort by modified date
		}
		ssed.entries[e.uuid] = e
		entriesToSortByModified[e.uuid] = e
		ssed.entryNameToUUID[e.Entry] = e.uuid
	}
	sortedEntries := make(timeSlice, 0, len(entriesToSortByModified))
	for _, d := range entriesToSortByModified {
		sortedEntries = append(sortedEntries, d)
	}
	sort.Sort(sortedEntries)

	alreadyAddedEntry := make(map[string]bool)
	var entriesToSortByCreated = make(map[string]Entry)
	for _, entry := range sortedEntries {
		if _, ok := alreadyAddedEntry[entry.Entry]; ok {
			continue
		}
		alreadyAddedEntry[entry.Entry] = true
		entry.datetime, _ = utils.ParseDate(entry.Timestamp) // sort by created date now
		entriesToSortByCreated[entry.uuid] = entry
	}
	sortedEntries = make(timeSlice, 0, len(entriesToSortByCreated))
	for _, d := range entriesToSortByCreated {
		sortedEntries = append(sortedEntries, d)
	}
	sort.Sort(sortedEntries)

	// put them in the ordering
	for _, entry := range sortedEntries {
		if val, ok := ssed.ordering[entry.Document]; !ok {
			ssed.ordering[entry.Document] = []string{entry.uuid}
		} else {
			ssed.ordering[entry.Document] = append(val, entry.uuid)
		}
	}

	// go in chronological order, so reverse list
	for key := range ssed.ordering {
		for i, j := 0, len(ssed.ordering[key])-1; i < j; i, j = i+1, j-1 {
			ssed.ordering[key][i], ssed.ordering[key][j] = ssed.ordering[key][j], ssed.ordering[key][i]
		}
	}
	ssed.parsed = true
}

// ListEntries returns slice of all the entries in all documents
func (ssed *Fs) ListEntries() []string {
	if !ssed.parsed {
		ssed.parseArchive()
	}
	entries := make([]string, len(ssed.entryNameToUUID))
	i := 0
	for entry := range ssed.entryNameToUUID {
		entries[i] = entry
		i++
	}
	return entries
}

// ListDocuments lists all documents available
func (ssed *Fs) ListDocuments() []string {
	defer timeTrack(time.Now(), "Listing documents")
	if !ssed.parsed {
		ssed.parseArchive()
	}
	documents := []string{}
	for document := range ssed.ordering {
		ignoring := false
		for _, uuid := range ssed.ordering[document] {
			if ssed.entries[uuid].Text == "ignore document" {
				ignoring = true
				break
			}
		}
		if !ignoring {
			documents = append(documents, document)
		}
	}
	sort.Strings(documents)
	return documents
}

func (ssed *Fs) entryExists(entryName string) bool {
	if !ssed.parsed {
		ssed.parseArchive()
	}
	if _, ok := ssed.entryNameToUUID[entryName]; ok {
		return true
	}
	return false
}

// GetDocumentOrEntry returns a entry slice that is either the entry or all entries in a document
// this is a useful function if you don't know whether an input is a document or an entry
func (ssed *Fs) GetDocumentOrEntry(ambiguous string) ([]Entry, bool, string, error) {
	if !ssed.parsed {
		ssed.parseArchive()
	}
	var entries []Entry
	logger.Debug("Ambiguous: %s", ambiguous)
	for document := range ssed.ordering {
		logger.Debug("Possible doc: %s", document)
		if ambiguous == document {
			return ssed.GetDocument(ambiguous), true, document, nil
		}
		for _, entryUUID := range ssed.ordering[document] {
			if ssed.entries[entryUUID].Entry == ambiguous {
				entries = []Entry{ssed.entries[entryUUID]}
				return entries, false, ssed.entries[entryUUID].Document, nil
			}
		}
	}
	return entries, true, ambiguous, errors.New("Can't find entry or document")
}

// GetDocument returns a slice of all entries in that document
func (ssed *Fs) GetDocument(documentName string) []Entry {
	defer timeTrack(time.Now(), "Getting document "+documentName)
	if !ssed.parsed {
		ssed.parseArchive()
	}
	entries := make([]Entry, len(ssed.ordering[documentName]))
	curEntry := 0
	for _, uuid := range ssed.ordering[documentName] {
		if ssed.entries[uuid].Text == "ignore document" {
			logger.Debug("Ignoring document %s", ssed.entries[uuid].Timestamp)
			return []Entry{}
		}
		if ssed.entries[uuid].Text == "ignore entry" {
			logger.Debug("Ignoring entry %s", ssed.entries[uuid].Timestamp)
			continue
		}
		entries[curEntry] = ssed.entries[uuid]
		curEntry++
	}
	return entries[0:curEntry]
}

// GetEntry returns the entry with specified name and document
func (ssed *Fs) GetEntry(documentName, entryName string) (Entry, error) {
	defer timeTrack(time.Now(), "Getting entry "+entryName)
	if !ssed.parsed {
		ssed.parseArchive()
	}

	var e Entry
	for _, uuid := range ssed.ordering[documentName] {
		log.Println(entryName, ssed.entries[uuid].Entry)
		if ssed.entries[uuid].Entry == entryName {
			if ssed.entries[uuid].Text == "ignore entry" {
				return e, errors.New("Entry deleted")
			} else {
				return ssed.entries[uuid], nil
			}
		}
	}
	return e, errors.New("Entry not found")
}

// func (ssed *Fs) Dump(filename string) {
// 	ssed.parseArchive()
// 	for docName := range ssed.ordering {
// 		var doc document
// 		doc.Name = docName
// 		doc.Entries = make([]Entry, len(ssed.ordering[docName]))
// 		for i, uuid := range ssed.ordering[docName] {
// 			doc.Entries[i] = ssed.entries[uuid]
// 		}
// 		bJson, _ := json.MarshalIndent(doc, "", " ")
// 		fmt.Println(string(bJson))
// 	}
// 	ioutil.WriteFile(filename, bJson, 0644)
// }

// DumpAll dumps a file with current date and name, encyprted
func (ssed *Fs) DumpAll() (string, error) {
	filename := ssed.username + "-" + time.Now().Format("2006-01-02") + ".bol"
	files, _ := filepath.Glob(path.Join(ssed.pathToLocalRepo, "*"))

	ssed.entries = make(map[string]Entry)
	var entriesToSortByModified = make(map[string]Entry)
	for _, file := range files {
		logger.Debug("Parsing %s", file)
		decrypted, err := utils.DecryptFromFile(ssed.password, file)
		if err != nil {
			panic(err)
		}
		var e Entry
		err = json.Unmarshal(decrypted, &e)
		if err != nil {
			panic(err)
		}
		e.uuid = filepath.Base(file)
		e.datetime, _ = utils.ParseDate(e.Timestamp)
		if len(e.ModifiedTimestamp) > 0 {
			e.datetime, _ = utils.ParseDate(e.ModifiedTimestamp) // sort by modified date
		}
		ssed.entries[e.uuid] = e
		entriesToSortByModified[e.uuid] = e
	}
	sortedEntries := make(timeSlice, 0, len(entriesToSortByModified))
	for _, d := range entriesToSortByModified {
		sortedEntries = append(sortedEntries, d)
	}
	sort.Sort(sortedEntries)

	// Add sorted entries to document
	documentMap := make(map[string]document)
	for _, e := range sortedEntries {
		if _, ok := documentMap[e.Document]; !ok {
			documentMap[e.Document] = document{Name: e.Document}
		}
		entries := documentMap[e.Document].Entries
		entries = append(entries, e)
		documentMap[e.Document] = document{Name: e.Document, Entries: entries}
	}

	// Make a array from them
	documentList := make([]document, len(documentMap))
	i := 0
	for doc := range documentMap {
		documentList[i] = documentMap[doc]
		i++
	}
	bJSON, _ := json.MarshalIndent(documentList, "", " ")
	utils.EncryptToFile(bJSON, ssed.password, filename)
	return filename, nil
}

// Import imports an unencrypted bol file
func (ssed *Fs) Import(filename string) error {
	bJSON, err := ioutil.ReadFile(filename)
	if err != nil {
		return err
	}
	var documentList []document
	err = json.Unmarshal(bJSON, &documentList)
	if err != nil {
		return err
	}

	for _, document := range documentList {
		for _, entry := range document.Entries {
			ssed.Update(entry.Text, entry.Document, entry.Entry, entry.ModifiedTimestamp)
		}
	}
	return nil
}

// timeTrack from https://coderwall.com/p/cp5fya/measuring-execution-time-in-go
func timeTrack(start time.Time, name string) {
	elapsed := time.Since(start)
	logger.Debug("%s took %s", name, elapsed)
}
