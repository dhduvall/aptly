package gcs

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"github.com/pkg/errors"
	"github.com/smira/aptly/aptly"
	"github.com/smira/aptly/utils"
	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"
	gcs "google.golang.org/api/storage/v1"
	"io"
	"log"
	"os"
	"path/filepath"
)

const (
	scope = gcs.DevstorageReadWriteScope
)

// PublishedStorage abstract file system with published files (actually hosted on GCS)
type PublishedStorage struct {
	service    *gcs.ObjectsService
	bucketName string
	prefix     string
}

func NewPublishedStorage(bucketName string, prefix string) (*PublishedStorage, error) {
	client, err := google.DefaultClient(context.Background(), scope)
	if err != nil {
		log.Fatalf("Unable to get default client: %v", err)
	}

	storageClient, err := gcs.New(client)
	if err != nil {
		log.Fatalf("Unable to create storage service: %v", err)
	}
	service := gcs.NewObjectsService(storageClient)

	return &PublishedStorage{service, bucketName, prefix}, nil
}

func (storage *PublishedStorage) String() string {
	return fmt.Sprintf("GCS: %s:%s", storage.bucketName, storage.prefix)
}

func (storage *PublishedStorage) MkDir(path string) error {
	// noop - GCS does not have <airquotes> directories </airquotes>
	return nil
}

func (storage *PublishedStorage) PutFile(path string, sourceFilename string) error {

	source, err := os.Open(sourceFilename)
	if err != nil {
		return err
	}
	defer source.Close()

	err = storage.putFile(path, source)
	if err != nil {
		return fmt.Errorf("error uploading %s to %s: %s", sourceFilename, storage, err)
	}
	return err
}

func (storage *PublishedStorage) putFile(path string, source io.ReadSeeker) error {

	object := &gcs.Object{Name: filepath.Join(storage.prefix, path)}

	_, err := storage.service.Insert(storage.bucketName, object).Media(source).Do()

	return err
}

// Remove removes single file under public path
func (storage *PublishedStorage) Remove(path string) error {
	err := storage.service.Delete(storage.bucketName, filepath.Join(storage.prefix, path)).Do()
	if err != nil {
		return fmt.Errorf("error deleting %s from %s: %s", path, storage, err)
	}
	return nil
}

func (storage *PublishedStorage) RemoveDirs(path string, progress aptly.Progress) error {
	fileList, err := storage.Filelist(path)
	if err != nil {
		return err
	}

	progress.InitBar(int64(len(fileList)), false)
	defer progress.ShutdownBar()

	for idx, fileName := range fileList {
		// Don't delete everything by accident
		objectName := filepath.Join(storage.prefix, path, fileName)
		storage.service.Delete(storage.bucketName, objectName)
		if idx%100 == 0 {
			progress.AddBar(100)
		}
	}
	return nil
}

func (storage *PublishedStorage) LinkFromPool(publishedDirectory string, baseName string, sourcePool aptly.PackagePool, sourcePath string, sourceChecksums utils.ChecksumInfo, force bool) error {

	relPath := filepath.Join(publishedDirectory, baseName)
	poolPath := filepath.Join(storage.prefix, relPath)

	var (
		dstKey *gcs.Object
		err    error
	)

	dstKey, err = storage.service.Get(storage.bucketName, poolPath).Do()

	if err == nil {
		// GCS gives us checksums encoded in Base64, not hex, so we need to convert to compare.
		md5raw, _ := base64.StdEncoding.DecodeString(dstKey.Md5Hash)
		destinationMD5 := hex.EncodeToString(md5raw)
		sourceMD5 := sourceChecksums.MD5


		if sourceMD5 == "" {
			return fmt.Errorf("unable to compare object, MD5 checksum missing")
		}
		if destinationMD5 == "" {
			return fmt.Errorf("No MD5 checksum on remote file '%s'; is it a composite object?", poolPath)
		}
		if destinationMD5 == sourceMD5 {
			return nil
		}

		if !force && destinationMD5 != sourceMD5 {
			return fmt.Errorf("error putting file to %s: file already exists and is different: %s", poolPath, storage)

		}
	}

	source, err := sourcePool.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()

	err = storage.putFile(relPath, source)
	if err != nil {
		err = errors.Wrap(err, fmt.Sprintf("error uploading %s to %s: %s", sourcePath, storage, poolPath))
	}
	return err
}

// Filelist returns list of files under prefix
func (storage *PublishedStorage) Filelist(prefix string) ([]string, error) {

	const page = 1000

	results := []string{}
	prefix = filepath.Join(storage.prefix, prefix)

	if prefix != "" {
		prefix += "/"
	}

	var pageToken string
	for {
		result, err := storage.service.List(storage.bucketName).MaxResults(page).PageToken(pageToken).Prefix(prefix).Do()
		pageToken = result.NextPageToken
		if err == nil {
			for _, obj := range result.Items {
				if prefix == "" {
					results = append(results, obj.Name)
				} else {
					results = append(results, obj.Name[len(prefix):])
				}
			}
		} else {
			return nil, err
		}
		if result.NextPageToken == "" {
			return results, nil
		}
	}

	panic("unreachable")
}

// RenameFile renames (moves) file.  GCS doesn't have a rename operation, so we
// do a copy/remove pair.
func (storage *PublishedStorage) RenameFile(oldName, newName string) error {

	sourcePath := filepath.Join(storage.prefix, oldName)
	destPath := filepath.Join(storage.prefix, newName)

	sourceObject, err := storage.service.Get(storage.bucketName, sourcePath).Do()

	if err != nil {
		return err
	}

	_, err = storage.service.Rewrite(storage.bucketName, sourcePath, storage.bucketName, destPath, sourceObject).Do()
	if err != nil {
		return err
	}

	err = storage.service.Delete(storage.bucketName, sourcePath).Do()
	return err
}

// SymLink creates a symbolic link, which can be read with ReadLink
func (storage *PublishedStorage) SymLink(src string, dst string) error {
	return fmt.Errorf("GCS doesn't support symbolic links")
}

// HardLink creates a hardlink of a file
func (storage *PublishedStorage) HardLink(src string, dst string) error {
	return fmt.Errorf("GCS doesn't support hard links")
}

// FileExists returns true if path exists
func (storage *PublishedStorage) FileExists(path string) (bool, error) {
	fullPath := filepath.Join(storage.prefix, path)

	getcall := storage.service.Get(storage.bucketName, fullPath)
	_, err := getcall.IfGenerationNotMatch(0).Do()
	if err == nil {
		return true, nil
	} else {
		return false, err
	}
}

// ReadLink returns the symbolic link pointed to by path
func (storage *PublishedStorage) ReadLink(path string) (string, error) {
	return "", fmt.Errorf("GCS doesn't support symbolic links")
}
