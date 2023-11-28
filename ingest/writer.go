package ingest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"scratchdb/client"
	"scratchdb/config"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/oklog/ulid/v2"
	"github.com/rs/zerolog/log"
	"github.com/tidwall/gjson"
)

type FileWriter struct {
	Client *client.Client

	// Where to save data
	DataDirectory string

	// Where in S3 to upload file
	UploadDirectory string

	// Extra metadata associated with each file
	Tags map[string]string

	Config *config.Config

	// Current file being written to
	fd *os.File

	// Push closed files to a longer term storage
	pusherDone chan bool

	// Ensure only 1 rotation is happening at a time
	rotating sync.Mutex
	// Ensure only 1 file write (or rotate) is happening at a time
	canWrite sync.Mutex

	// Used to rotate every x interval
	ticker *time.Ticker

	tickerDone chan bool

	wg sync.WaitGroup
}

func NewFileWriter(
	DataDirectory string,
	config *config.Config,
	UploadDirectory string,
	Tags map[string]string,
) *FileWriter {
	fw := &FileWriter{
		Client:          client.NewClient(config),
		DataDirectory:   DataDirectory,
		Config:          config,
		ticker:          time.NewTicker(time.Duration(config.Ingest.MaxAgeSeconds) * time.Second),
		tickerDone:      make(chan bool),
		pusherDone:      make(chan bool),
		UploadDirectory: UploadDirectory,
		Tags:            Tags,
	}

	closedDir := filepath.Join(fw.DataDirectory, "closed")
	err := os.MkdirAll(closedDir, os.ModePerm)
	if err != nil {
		log.Err(err).Send()
	}

	openDir := filepath.Join(fw.DataDirectory, "open")
	err = os.MkdirAll(openDir, os.ModePerm)
	if err != nil {
		log.Err(err).Send()
	}

	// Kickstart the writer by creating a new file
	fw.Rotate(true)

	// Kickstart automatic file rotation on timer
	fw.wg.Add(1)
	go fw.rotateOnTimer()

	fw.wg.Add(1)
	go fw.pushFiles()

	return fw
}

func (f *FileWriter) rotateOnTimer() {
	defer f.wg.Done()

	for {
		select {
		case <-f.tickerDone:
			log.Info().Msgf("Stopping ticker for %s", f.DataDirectory)
			return
		case <-f.ticker.C:
			//log.Debug().Msg("Trying periodic rotate...")

			f.canWrite.Lock()
			fileinfo, err := f.fd.Stat()
			if err != nil {
				log.Err(err).Msg("Unable to auto rotate")
			}
			if fileinfo.Size() > 0 {
				f.Rotate(true)
			}
			f.canWrite.Unlock()
		}
	}
}

func (f *FileWriter) uploadS3File(filename string) error {
	path := filepath.Join(f.DataDirectory, "closed", filename)
	//log.Debug().Msgf("Uploading %s %s", path, "to s3")
	file, err := os.Open(path)
	if err != nil {
		log.Printf("os.Open - filename: %s, err: %v", path, err)
		return err
	}
	defer file.Close()

	s3Key := filepath.Join(f.UploadDirectory, filename)
	_, err = f.Client.S3.PutObject(&s3.PutObjectInput{
		Bucket:             aws.String(f.Config.Storage.S3Bucket),
		Key:                aws.String(s3Key),
		Body:               file,
		ContentDisposition: aws.String("attachment"),
	})
	if err != nil {
		return err
	}

	sqsMessage := make(map[string]string)
	for k, v := range f.Tags {
		log.Debug().Msgf("Adding kv to sqs message %s %s", k, v)
		sqsMessage[k] = v
	}
	sqsMessage["bucket"] = f.Config.Storage.S3Bucket
	sqsMessage["key"] = s3Key
	log.Debug().Msgf("Final SQS message %s", sqsMessage)

	sqsPayload, err := json.Marshal(sqsMessage)
	if err != nil {
		return err
	}
	log.Debug().Msgf("SQS JSON Payload %s", string(sqsPayload))

	_, err = f.Client.SQS.SendMessage(
		&sqs.SendMessageInput{
			MessageBody: aws.String(string(sqsPayload)),
			QueueUrl:    &f.Config.AWS.SQS,
		})

	return err
}

// TODO: Ideally want to have a pool of workers who can upload
func (f *FileWriter) pushFiles() {
	defer f.wg.Done()

	keepReading := true
	for keepReading {
		select {
		case <-f.pusherDone:
			log.Info().Msg("Finishing uploading remaining files, then will stop")
			keepReading = false
		default:
		}

		//log.Debug().Msg("Checking for files to upload")

		uploadPath := filepath.Join(f.DataDirectory, "closed")
		entries, err := os.ReadDir(uploadPath)
		if err != nil {
			log.Err(err).Send()
			continue
		}

		for _, e := range entries {
			filename := filepath.Join(uploadPath, e.Name())
			fileinfo, err := e.Info()

			if err != nil {
				log.Err(err).Msgf("Unable to get info for file %s", filename)
			}

			var uploadError error
			if fileinfo.Size() > 0 {
				uploadError = f.uploadS3File(e.Name())
			}

			if uploadError == nil {
				err = os.Remove(filename)
				if err != nil {
					log.Err(err).Msgf("Unable to remove file %s", filename)
				}
			} else {
				log.Fatal().Err(uploadError).Msg("Unable to upload")
			}
		}

		time.Sleep(1 * time.Second)
	}
}

func (f *FileWriter) Rotate(createNew bool) error {
	// BLOCKS ALL WRITES while we rotate.
	// Could we be more clever here by opening the new file
	// in a new goroutine to continue write while we close the previous

	// Make sure only one rotation is happening at a time, as we do them asynchronously

	rotating := f.rotating.TryLock()
	if !rotating {
		log.Debug().Msg("Someone else is currently rotating, skipping this rotation")
		return nil
	}
	defer f.rotating.Unlock()

	// log.Debug().Msg("Rotating!")
	var err error

	// Check to see if we have an open fd
	if f.fd != nil {
		fileinfo, err := f.fd.Stat()
		if err != nil {
			log.Err(err).Send()
			return err
		}

		oldName := f.fd.Name()
		filename := fileinfo.Name()

		err = f.fd.Close()
		if err != nil {
			log.Err(err).Send()
			return err
		}

		newDir := filepath.Join(f.DataDirectory, "closed")
		err = os.MkdirAll(newDir, os.ModePerm)
		if err != nil {
			log.Err(err).Send()
			return err
		}

		err = os.Rename(oldName, filepath.Join(newDir, filename))
		if err != nil {
			log.Err(err).Send()
			return err
		}
	}

	if createNew {
		newFileId := ulid.Make().String()
		dir := filepath.Join(f.DataDirectory, "open")
		err = os.MkdirAll(dir, os.ModePerm)
		if err != nil {
			log.Err(err).Send()
			return err
		}

		path := filepath.Join(dir, newFileId+".ndjson")

		f.fd, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Err(err).Send()
			return err
		}
	}

	return nil
}

func (f *FileWriter) Write(data string) error {
	var err error

	f.canWrite.Lock()
	defer f.canWrite.Unlock()

	// check to see if we will hit our file size limit
	fileinfo, err := f.fd.Stat()
	if err != nil {
		log.Err(err).Send()
		return err
	}
	if fileinfo.Size()+int64(len(data)) > f.Config.Ingest.MaxSizeBytes {
		err = f.Rotate(true)
	}
	if err != nil {
		log.Err(err).Send()
		return err
	}

	// Create metadata
	_, currentFileName := filepath.Split(f.fd.Name())
	metaData := fmt.Sprintf(`{"__row_id": "%s", "__batch_file": "%s"}`, ulid.Make().String(), currentFileName)
	combined := gjson.Get(`[`+data+`,`+metaData+`]`, `@join.@ugly`).Raw

	// write data
	_, err = f.fd.WriteString(combined + "\n")
	if err != nil {
		log.Err(err).Send()
	}

	return err
}

func (f *FileWriter) Close() error {
	f.ticker.Stop()
	f.tickerDone <- true

	// Close open file
	f.canWrite.Lock()
	err := f.Rotate(false)
	f.canWrite.Unlock()

	// Check on this error
	if err != nil {
		log.Err(err).Send()
	}

	log.Info().Msg("Finishing uploading files")
	f.pusherDone <- true

	f.wg.Wait()

	return nil
}
