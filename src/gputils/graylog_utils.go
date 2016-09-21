package gputils

import (
	// standard packages
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	// external packages
	"github.com/boltdb/bolt"
	"gopkg.in/fsnotify.v1"
)

type Message struct {
	ID         int
	Version    string
	Message    string `json:"short_message"`
	Host       string `json:"host"`
	Level      int    `json:"level"`
	MessageLog string `json:"_log"`
	File       string `json:"_file"`
	ArchiveDir string `json:"_archivedir"`
	Localtime  string `json:"_localtime"`
}

type BoltDb struct {
	Dbfile     string `toml:"dbfile"`
	DB         *bolt.DB
	writerChan chan [3]interface{}
	graylog    Graylog
}

type Graylog struct {
	Url      string `toml:"url"`
	Port     int    `toml:"port"`
	Format   string `toml:"format"`
	Protocol string `toml:"protocol"`
}

type Dir_watcher struct {
	Watcher_type      string
	Name              string
	Directory_log     string
	Ext_file          string
	Directory_archive string
	Payload_host      string
	Payload_level     int
}

func LoopDirectoryLog(boltdb *BoltDb, graylog *Graylog, dir_watcher Dir_watcher) {

	go func() {
		for {
			ListFilesAndPush(boltdb, graylog, dir_watcher)
			time.Sleep(60000 * time.Millisecond)
		}
	}()

}

func (this *BoltDb) Resend(bucket string) {
	go func() {
		for {
			this.DB.View(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte(bucket))
				if b == nil {
					return nil
				}
				b.ForEach(func(k, v []byte) error {
					//fmt.Printf("key=%d, value=%s\n", k, v)
					//find a payload
					payload := this.findOne(bucket, k)

					// delete payload from boltdb
					this.purgeOne(bucket, k)

					// resend

					fmt.Printf("Resend findone Message : %d %s %s %s %s\n", payload.ID, payload.Message, payload.Host, payload.File, payload.MessageLog)

					this.pushPayload(payload)

					return nil
				})
				//time.Sleep(5000 * time.Millisecond)
				return nil
			})
			time.Sleep(15000 * time.Millisecond)
		}
	}()
}

func (this *BoltDb) pushPayload(payload *Message) {

	fmt.Printf("pushPayload : %s\n", payload)

	url := this.graylog.Protocol + "://" + this.graylog.Url + ":" + strconv.Itoa(this.graylog.Port) + "/" + this.graylog.Format

	fmt.Printf("PushPayload graylog : %s\n", url)

	go PushToGraylogHttp(
		"event",
		this,
		url,
		payload)

}

func (this *BoltDb) findOne(bucket string, key []byte) *Message {

	var payload Message

	this.DB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		v := b.Get(key)
		// fmt.Printf("findone data : %s\n", v)

		if err := json.Unmarshal(v, &payload); err != nil {
			return nil
		}
		return nil
	})
	return &payload
}

func (this *BoltDb) Purge(bucket string) {
	this.DB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		b.ForEach(func(k, v []byte) error {
			fmt.Printf("key=%s, value=%s\n", k, v)
			b.Delete(k)
			return nil
		})
		return nil
	})
}

func (this *BoltDb) purgeOne(bucket string, key []byte) {
	this.DB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		b.Delete(key)
		return nil
	})
}

func (this *BoltDb) writer() {
	for data := range this.writerChan {
		bucket := data[0].(string)
		keyId := data[1].(string)
		data := data[2].(*Message)
		err := this.DB.Update(func(tx *bolt.Tx) error {
			sessionBucket, err := tx.CreateBucketIfNotExists([]byte(bucket))
			if err != nil {
				return fmt.Errorf("create bucket: %s", err)
			}

			if keyId == "" {
				id, _ := sessionBucket.NextSequence()
				data.ID = int(id)
				fmt.Println("payload ID : ", data.ID)

				// Marshal user data into bytes.
				buf, err := json.Marshal(data)
				if err != nil {
					return err
				}
				return sessionBucket.Put(itob(data.ID), buf)

			} else {
				// Marshal user data into bytes.
				buf, err := json.Marshal(data)
				if err != nil {
					return err
				}
				return sessionBucket.Put([]byte(keyId), buf)
			}

		})
		if err != nil {
			// TODO: Handle instead of panic
			panic(err)
		}
	}
}

func NewBoltDb(dbPath string, graylogconf Graylog) *BoltDb {
	db, err := bolt.Open(dbPath, 0666, nil)
	writerChan := make(chan [3]interface{})
	boltDb := &BoltDb{DB: db, writerChan: writerChan, graylog: graylogconf}
	go boltDb.writer()
	if err != nil {
		panic(err)
	}
	return boltDb
}

func cp(dst, src string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	// no need to check errors on read only file, we already got everything
	// we need from the filesystem, so nothing can go wrong now.
	defer s.Close()
	d, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(d, s); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

func ListFilesAndPush(boltdb *BoltDb, graylog *Graylog, dir_watcher Dir_watcher) {

	url := graylog.Protocol + "://" + graylog.Url + ":" + strconv.Itoa(graylog.Port) + "/" + graylog.Format

	files, _ := ioutil.ReadDir(dir_watcher.Directory_log)
	for _, f := range files {
		file_ext := filepath.Ext(f.Name())
		if file_ext == dir_watcher.Ext_file {
			fmt.Println("file to push : ", dir_watcher.Directory_log+"/"+f.Name())

			data, err := ioutil.ReadFile(dir_watcher.Directory_log + "/" + f.Name())
			if err != nil {
				panic(err)
			}
			log.Println("data: ", string(data))

			payload := payload(dir_watcher.Directory_archive, dir_watcher.Name, string(data), dir_watcher.Directory_log+"/"+f.Name(), dir_watcher.Payload_host, dir_watcher.Payload_level)

			go PushToGraylogHttp(
				dir_watcher.Watcher_type,
				boltdb,
				url,
				&payload)
		}
	}

}

func payload(archive_dir string, msg string, messagelog string, file string, host string, level int) Message {

	t := time.Now()
	m := Message{
		Version:    "1.1",
		Message:    msg,
		Host:       host,
		Level:      level,
		MessageLog: messagelog,
		File:       file,
		ArchiveDir: archive_dir,
		Localtime:  t.Format("01-02-2006T15-04-05"),
	}

	return m
}

func PushToGraylogHttp(watcher_type string, boltdb *BoltDb, url string, payload *Message) {

	payload_id := payload.ID
	archive := payload.ArchiveDir
	file := payload.File

	file_name := filepath.Base(file)
	log.Println("file name :", file_name)

	//url := "http://192.168.51.57:12201/gelf"
	fmt.Println("URL:>", url)
	fmt.Println("PAYLOAD ID:>", payload_id)

	jsonStr, _ := json.Marshal(payload)

	fmt.Println("json: ", string(jsonStr))

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonStr))
	//    req.Header.Set("X-Custom-Header", "myvalue")
	//    req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		//panic(err)
		//log.Fatal("err")
		log.Println("graylog post err  :", err)
		if watcher_type == "event" {
			boltdb.writerChan <- [3]interface{}{"graylog", "", payload}
		}
		return
	}
	defer resp.Body.Close()

	fmt.Println("response Status:", resp.Status)
	fmt.Println("response Headers:", resp.Header)
	body, _ := ioutil.ReadAll(resp.Body)
	fmt.Println("response Body:", string(body))

	if strings.Contains(resp.Status, "202") == true {
		log.Println("status: ", resp.Status)

		t := time.Now()
		file_timestamp := t.Format("01-02-2006T15-04-05") + "-" + strconv.Itoa(t.Nanosecond())

		file_name += "_" + file_timestamp
		//fmt.Println("timestamp : ", file_timestamp)

		err := cp(archive+"/"+file_name+".txt", file)
		if err != nil {
			log.Fatal(err)
		} else {
			os.Remove(file)
		}
	} else {
		log.Fatal("Graylog server error : ", resp.Status)
		// store payload to boltdb to send it later
		//storeDB(dbfile, &m)

		if watcher_type == "event" {
			boltdb.writerChan <- [3]interface{}{"graylog", "", payload}
		}
	}
}

func LogNewWatcher(boltdb *BoltDb, graylog *Graylog, dir_watcher Dir_watcher) {

	fmt.Println("watched dir :", dir_watcher.Directory_log)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()

	os.Mkdir(dir_watcher.Directory_archive, 0700)

	done := make(chan bool)
	go func() {
		for {
			select {
			case event := <-watcher.Events:
				log.Println("event:", event)
				if event.Op&fsnotify.Create == fsnotify.Create {
					log.Println("created file:", event.Name)
					file_ext := filepath.Ext(event.Name)

					if file_ext == dir_watcher.Ext_file {

						data, err := ioutil.ReadFile(event.Name)
						if err != nil {
							panic(err)
						}
						log.Println("data: ", string(data))

						//var sem = make(chan int, MaxTaches)
						url := graylog.Protocol + "://" + graylog.Url + ":" + strconv.Itoa(graylog.Port) + "/" + graylog.Format
						// fmt.Println("URL : " + url)

						payload := payload(dir_watcher.Directory_archive, dir_watcher.Name, string(data), event.Name, dir_watcher.Payload_host, dir_watcher.Payload_level)

						go PushToGraylogHttp(
							dir_watcher.Watcher_type,
							boltdb,
							url,
							&payload)
					}
				}
			case err := <-watcher.Errors:
				log.Println("error:", err)
			}
		}
	}()

	err = watcher.Add(dir_watcher.Directory_log)
	if err != nil {
		log.Fatal(err)
	}
	<-done
}

// not used
func storeDB(dbfile string, m *Message) {
	db, err := bolt.Open(dbfile, 0600, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// store json to db
	db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("graylog"))
		if err != nil {
			return fmt.Errorf("create bucket: %s", err)
		}

		id, _ := b.NextSequence()
		m.ID = int(id)

		// Marshal user data into bytes.
		buf, err := json.Marshal(m)
		if err != nil {
			return err
		}
		fmt.Println("payload ID : ", m.ID)
		// Persist bytes to users bucket.
		return b.Put(itob(m.ID), buf)
		//return b.Put([]byte("test"), []byte("My New Year post"))
	})

}

// itob returns an 8-byte big endian representation of v.
func itob(v int) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(v))
	return b
}
