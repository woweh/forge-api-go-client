package dm

// Instructions for the S3 update:
// https://forge.autodesk.com/blog/data-management-oss-object-storage-service-migrating-direct-s3-approach
/*
		Direct-to-S3 approach for Data Management OSS
		To upload and download files, applications must generate a signed URL, then upload or download the binary. Here are the steps (pseudo code):

		Upload
		========

		1. Calculate the number of parts of the file to upload
			Note: Each uploaded part except for the last one must be at least 5MB (1024 * 5)

		2. Generate up to 25 URLs for uploading specific parts of the file using the
		   GET buckets/:bucketKey/objects/:objectKey/signeds3upload?firstPart=<index of first part>&parts=<number of parts>
	       endpoint.
			a) The part numbers start with 1
			b) For example, to generate upload URLs for parts 10 through 15, set firstPart to 10 and parts to 6
			c) This endpoint also returns an uploadKey that is used later to request additional URLs or to finalize the upload

		3. Upload remaining parts of the file to their corresponding upload URLs
			a) Consider retrying (for example, with an exponential backoff) individual uploads when the response code is 100-199, 429, or 500-599
			b) If the response code is 403, the upload URLs have expired; go back to step #2
			c) If you have used up all the upload URLs and there are still parts that must be uploaded, go back to step #2

		4. Finalize the upload using the POST buckets/:bucketKey/objects/:objectKey/signeds3upload endpoint, using the uploadKey value from step #2
*/

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/thedevsaddam/retry"
)

const (
	// megaByte is 1048576 byte
	megaByte = 1 << 20
	// maxParts is the maximum number of parts returned by the "signeds3upload" endpoint.
	maxParts = 25
	// signedS3UploadEndpoint is the name of the signeds3upload endpoint.
	signedS3UploadEndpoint = "signeds3upload"
	// minutesExpiration is the expiration period of the signed upload URLs.
	// Autodesk default is 2 minutes (1 to 60 minutes).
	minutesExpiration = 60
)

var (
	// defaultSize is the default size of download/upload chunks.
	defaultSize = int64(100 * megaByte)
)

func newUploadJob(api BucketAPI, bucketKey, objectName, fileToUpload string) (job uploadJob, err error) {

	job = uploadJob{}
	job.api = api
	job.bucketKey = bucketKey
	job.objectKey = objectName
	job.fileToUpload = fileToUpload
	job.minutesExpiration = minutesExpiration
	job.uploadKey = ""

	fileInfo, err := os.Stat(job.fileToUpload)
	if err != nil {
		return
	}

	// Determine the required number of parts
	// - In the examples, typically a chunk size of 5 or 10 MB is used.
	// - In the old API, the boundary for multipart uploads was 100 MB.
	//   => See const defaultSize

	job.fileSize = fileInfo.Size()
	job.totalParts = int((job.fileSize / defaultSize) + 1)
	job.numberOfBatches = (job.totalParts / maxParts) + 1

	return
}

func (job *uploadJob) uploadFile() (result UploadResult, err error) {

	file, err := os.Open(job.fileToUpload)
	if err != nil {
		return
	}
	defer file.Close()

	partsCounter := 0
	for i := 0; i < job.numberOfBatches; i++ {

		firstPart := (i * maxParts) + 1

		parts := job.getParts(partsCounter)

		// generate signed S3 upload url(s)
		tmpResult, err := retry.Do(3, 3*time.Second, job.getSignedUploadUrls, firstPart, parts)
		if err != nil {
			err = fmt.Errorf("Error getting signed URLs for parts %v-%v :\n%w", firstPart, parts, err)
			return
		}
		uploadUrls, _ := tmpResult[0].(signedUploadUrls)

		if i == 0 {
			// remember the uploadKey when requesting signed URLs for the first time
			job.uploadKey = uploadUrls.UploadKey
		}

		// upload the file in chunks to the signed url(s)
		for _, url := range uploadUrls.Urls {

			// read a chunk of the file
			bytesSlice := make([]byte, defaultSize)

			bytesRead, err := file.Read(bytesSlice)
			if err != nil {
				if err != io.EOF {
					err = fmt.Errorf("Error reading the file to upload:\n%w", err)
					return
				}
				// EOF reached
			}

			// upload the chunk to the signed URL
			if bytesRead > 0 {
				buffer := bytes.NewBuffer(bytesSlice[:bytesRead])
				_, err = retry.Do(3, 3*time.Second, uploadChunk, url, buffer)
				if err != nil {
					err = fmt.Errorf("Error uploading a chunk to URL:\n- %v\n%w", url, err)
					return
				}
			}
		}

		partsCounter += maxParts
	}

	// complete the upload
	tmpResult, err := retry.Do(3, 3*time.Second, job.completeUpload)
	if err != nil {
		err = fmt.Errorf("Error completing the upload:\n%w", err)
		return
	}
	result, _ = tmpResult[0].(UploadResult)

	return
}

// getParts gets the number of parts that must be processed in this batch.
func (job *uploadJob) getParts(partsCounter int) int {

	parts := maxParts

	if job.totalParts < (partsCounter + maxParts) {
		// Say totalParts = 20:  part[0]=20, firstPart[0]=1
		// Say totalParts = 30:  part[0]=25, firstPart[0]=1, part[1]= 5, firstPart[1]=26
		// Say totalParts = 40:  part[0]=25, firstPart[0]=1, part[1]=15, firstPart[1]=26
		// Say totalParts = 50:  part[0]=25, firstPart[0]=1, part[1]=25, firstPart[1]=26
		parts = job.totalParts - partsCounter
	}

	return parts
}

// getSignedUploadUrls calls the signedS3UploadEndpoint
func (job *uploadJob) getSignedUploadUrls(firstPart, parts int) (result signedUploadUrls, err error) {

	// - https://forge.autodesk.com/en/docs/data/v2/reference/http/buckets-:bucketKey-objects-:objectKey-signeds3upload-GET/

	// - https://forge.autodesk.com/en/docs/data/v2/tutorials/upload-file/#step-4-generate-a-signed-s3-url
	// - https://forge.autodesk.com/en/docs/data/v2/tutorials/app-managed-bucket/#step-2-initiate-a-direct-to-s3-multipart-upload

	accessToken, err := job.authenticate()
	if err != nil {
		return
	}

	// request the signed urls
	req, err := http.NewRequest("GET", job.getSignedS3UploadPath(), nil)
	if err != nil {
		return
	}

	addOrSetHeader(req, "Authorization", "Bearer "+accessToken)

	// appending to existing query args
	q := req.URL.Query()
	if job.uploadKey != "" {
		q.Add("uploadKey", job.uploadKey)
	}
	q.Add("firstPart", strconv.Itoa(firstPart))
	q.Add("parts", strconv.Itoa(parts))
	q.Add("minutesExpiration", strconv.Itoa(job.minutesExpiration))
	// assign encoded query string to http request
	req.URL.RawQuery = q.Encode()

	task := http.Client{}
	response, err := task.Do(req)
	if err != nil {
		return
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		content, _ := ioutil.ReadAll(response.Body)
		err = errors.New("[" + strconv.Itoa(response.StatusCode) + "] " + string(content))
		return
	}

	err = json.NewDecoder(response.Body).Decode(&result)

	return
}

// uploadChunk uploads a chunk of bytes to a given signedUrl.
func uploadChunk(signedUrl string, buffer *bytes.Buffer) (err error) {

	req, err := http.NewRequest("PUT", signedUrl, buffer)
	if err != nil {
		return
	}

	l := buffer.Len()
	req.ContentLength = int64(l)
	addOrSetHeader(req, "Content-Type", "application/octet-stream")
	addOrSetHeader(req, "Content-Length", strconv.Itoa(l))

	task := http.Client{}
	response, err := task.Do(req)
	if err != nil {
		return
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusOK {
		return
	}

	content, _ := ioutil.ReadAll(response.Body)
	err = errors.New("[" + strconv.Itoa(response.StatusCode) + "] " + string(content))

	return
}

// completeUpload instructs OSS to complete the object creation process after the bytes have been uploaded directly to S3.
// An object will not be accessible until this endpoint is called.
// This endpoint must be called within 24 hours of the upload beginning, otherwise the object will be discarded, and the upload must begin again from scratch.
func (job *uploadJob) completeUpload() (result UploadResult, err error) {

	// - https://forge.autodesk.com/en/docs/data/v2/reference/http/buckets-:bucketKey-objects-:objectKey-signeds3upload-POST/

	// - https://forge.autodesk.com/en/docs/data/v2/tutorials/upload-file/#step-6-complete-the-upload
	// - https://forge.autodesk.com/en/docs/data/v2/tutorials/app-managed-bucket/#step-4-complete-the-upload

	accessToken, err := job.authenticate()
	if err != nil {
		return
	}

	// size	integer: The expected size of the uploaded object.
	// If provided, OSS will check this against the blob in S3 and return an error if the size does not match.
	bodyData := struct {
		UploadKey string `json:"uploadKey"`
		Size      int    `json:"size"`
	}{
		UploadKey: job.uploadKey,
		Size:      int(job.fileSize),
	}

	bodyJson, err := json.Marshal(bodyData)
	if err != nil {
		return
	}

	req, err := http.NewRequest("POST", job.getSignedS3UploadPath(), bytes.NewBuffer(bodyJson))
	if err != nil {
		return
	}

	addOrSetHeader(req, "Authorization", "Bearer "+accessToken)
	addOrSetHeader(req, "Content-Type", "application/json")
	addOrSetHeader(req, "x-ads-meta-Content-Type", "application/octet-stream")

	task := http.Client{}
	response, err := task.Do(req)
	if err != nil {
		return
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		content, _ := ioutil.ReadAll(response.Body)
		err = errors.New("[" + strconv.Itoa(response.StatusCode) + "] " + string(content))
		return
	}

	err = json.NewDecoder(response.Body).Decode(&result)

	return
}

func addOrSetHeader(req *http.Request, key, value string) {
	if req.Header.Get(key) == "" {
		req.Header.Add(key, value)
	} else {
		req.Header.Set(key, value)
	}
}

func (job *uploadJob) getSignedS3UploadPath() string {
	// https://developer.api.autodesk.com/oss/v2/buckets/:bucketKey/objects/:objectKey/signeds3upload
	// :bucketKey/objects/:objectKey/signeds3upload
	return job.api.Authenticator.GetHostPath() + path.Join(job.api.BucketAPIPath, job.bucketKey, "objects", job.objectKey, signedS3UploadEndpoint)
}

func (job *uploadJob) authenticate() (accessToken string, err error) {
	bearer, err := job.api.Authenticator.GetToken("data:write data:read")
	if err != nil {
		return
	}
	accessToken = bearer.AccessToken
	return
}
