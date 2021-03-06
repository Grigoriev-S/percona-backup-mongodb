package backup

import (
	"compress/gzip"
	"io"
	"io/ioutil"
	"os"
	"path"
	"runtime"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/golang/snappy"
	"github.com/klauspost/compress/s2"
	"github.com/klauspost/pgzip"
	"github.com/minio/minio-go"
	"github.com/pierrec/lz4"
	"github.com/pkg/errors"

	"github.com/percona/percona-backup-mongodb/pbm"
)

// NopCloser wraps an io.Witer as io.WriteCloser
// with noop Close
type NopCloser struct {
	io.Writer
}

// Close to satisfy io.WriteCloser interface
func (NopCloser) Close() error { return nil }

// Compress makes a compressed the writer from given one
func Compress(w io.Writer, compression pbm.CompressionType) io.WriteCloser {
	switch compression {
	case pbm.CompressionTypeGZIP:
		return gzip.NewWriter(w)
	case pbm.CompressionTypePGZIP:
		pgw := pgzip.NewWriter(w)
		cc := runtime.NumCPU() / 2
		if cc == 0 {
			cc = 1
		}
		pgw.SetConcurrency(1<<20, cc)
		return pgw
	case pbm.CompressionTypeLZ4:
		return lz4.NewWriter(w)
	case pbm.CompressionTypeSNAPPY:
		return snappy.NewBufferedWriter(w)
	case pbm.CompressionTypeS2:
		cc := runtime.NumCPU() / 3
		if cc == 0 {
			cc = 1
		}
		return s2.NewWriter(w, s2.WriterConcurrency(cc))
	default:
		return NopCloser{w}
	}
}

// Save writes data to given store
func Save(data io.Reader, stg pbm.Storage, name string) error {
	switch stg.Type {
	case pbm.StorageFilesystem:
		filepath := path.Join(stg.Filesystem.Path, name)
		fw, err := os.Create(filepath)
		if err != nil {
			return errors.Wrapf(err, "create destination file <%s>", filepath)
		}
		_, err = io.Copy(fw, data)
		return errors.Wrap(err, "write to file")
	case pbm.StorageS3:
		switch stg.S3.Provider {
		default:
			awsSession, err := session.NewSession(&aws.Config{
				Region:   aws.String(stg.S3.Region),
				Endpoint: aws.String(stg.S3.EndpointURL),
				Credentials: credentials.NewStaticCredentials(
					stg.S3.Credentials.AccessKeyID,
					stg.S3.Credentials.SecretAccessKey,
					"",
				),
				S3ForcePathStyle: aws.Bool(true),
			})
			if err != nil {
				return errors.Wrap(err, "create AWS session")
			}
			cc := runtime.NumCPU() / 2
			if cc == 0 {
				cc = 1
			}
			_, err = s3manager.NewUploader(awsSession, func(u *s3manager.Uploader) {
				u.PartSize = 10 * 1024 * 1024 // 10MB part size
				u.LeavePartsOnError = true    // Don't delete the parts if the upload fails.
				u.Concurrency = cc
			}).Upload(&s3manager.UploadInput{
				Bucket: aws.String(stg.S3.Bucket),
				Key:    aws.String(path.Join(stg.S3.Prefix, name)),
				Body:   data,
			})
			return errors.Wrap(err, "upload to S3")
		case pbm.S3ProviderGCS:
			// using minio client with GCS because it
			// allows to disable chuncks muiltipertition for upload
			mc, err := minio.NewWithRegion(pbm.GCSEndpointURL, stg.S3.Credentials.AccessKeyID, stg.S3.Credentials.SecretAccessKey, true, stg.S3.Region)
			if err != nil {
				return errors.Wrap(err, "NewWithRegion")
			}
			_, err = mc.PutObject(stg.S3.Bucket, path.Join(stg.S3.Prefix, name), data, -1, minio.PutObjectOptions{})
			return errors.Wrap(err, "upload to GCS")
		}
	case pbm.StorageBlackHole:
		_, err := io.Copy(ioutil.Discard, data)
		return errors.Wrap(err, "upload to blackhole")
	default:
		return errors.New("unknown storage type")
	}
}
