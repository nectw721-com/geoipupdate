package geoipupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"

	"github.com/maxmind/geoipupdate/v6/internal"
	"github.com/maxmind/geoipupdate/v6/internal/geoipupdate/database"
)

// TestUpdaterOutput makes sure that the Updater outputs the result of its
// operation to stdout in json format.
func TestUpdaterOutput(t *testing.T) {
	now := time.Now().Truncate(time.Second).In(time.UTC)
	testTime := time.Date(2023, 4, 27, 12, 4, 48, 0, time.UTC)
	databases := []database.ReadResult{
		{
			EditionID:  "GeoLite2-City",
			OldHash:    "A",
			NewHash:    "B",
			ModifiedAt: testTime,
		}, {
			EditionID:  "GeoIP2-Country",
			OldHash:    "C",
			NewHash:    "D",
			ModifiedAt: testTime,
		},
	}

	tempDir := t.TempDir()

	config := &Config{
		EditionIDs:  []string{"GeoLite2-City", "GeoLite2-Country"},
		LockFile:    filepath.Join(tempDir, ".geoipupdate.lock"),
		Output:      true,
		Parallelism: 1,
	}

	// capture the output of the `output` logger.
	logOutput := &bytes.Buffer{}

	// create a fake Updater with a mocked database reader and writer.
	u := &Updater{
		config: config,
		reader: &mockReader{i: 0, result: databases},
		output: log.New(logOutput, "", 0),
		writer: &mockWriter{},
	}

	err := u.Run(context.Background())
	require.NoError(t, err)

	// make sure the expected output matches the input.
	var outputDatabases []database.ReadResult
	err = json.Unmarshal(logOutput.Bytes(), &outputDatabases)
	require.NoError(t, err)
	require.Equal(t, len(outputDatabases), len(databases))

	for i := 0; i < len(databases); i++ {
		require.Equal(t, databases[i].EditionID, outputDatabases[i].EditionID)
		require.Equal(t, databases[i].OldHash, outputDatabases[i].OldHash)
		require.Equal(t, databases[i].NewHash, outputDatabases[i].NewHash)
		require.Equal(t, databases[i].ModifiedAt, outputDatabases[i].ModifiedAt)
		// comparing time wasn't supported with require in older go versions.
		if !afterOrEqual(outputDatabases[i].CheckedAt, now) {
			t.Errorf("database %s was not updated", outputDatabases[i].EditionID)
		}
	}

	// Test with a write error.

	u.reader.(*mockReader).i = 0

	streamErr := http2.StreamError{
		Code: http2.ErrCodeInternal,
	}
	u.writer = &mockWriter{
		WriteFunc: func(_ *database.ReadResult) error {
			return streamErr
		},
	}

	err = u.Run(context.Background())
	require.ErrorIs(t, err, streamErr)
}

func TestRetryWhenWriting(t *testing.T) {
	tempDir := t.TempDir()

	databaseDir := filepath.Join(tempDir, "databases")

	// Create a databases folder
	err := os.MkdirAll(databaseDir, 0o750)
	require.NoError(t, err)

	try := 0
	sv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mocking the metadata endpoint.
		if r.URL.Path == "/geoip/updates/metadata" {
			w.Header().Set("Content-Type", "application/json")

			// The md5 here bleongs to the tar.gz sent below.
			metadata := []byte(
				`{"databases":[{"edition_id":"foo-db-name",` +
					`"md5":"83e01ba43c2a66e30cb3007c1a300c78","date":"2023-04-27"}]}`)
			_, err := w.Write(metadata)
			require.NoError(t, err)

			return
		}

		w.Header().Set("Last-Modified", "Wed, 27 Apr 2023 12:04:48 GMT")

		gzWriter := gzip.NewWriter(w)
		defer gzWriter.Close()
		tarWriter := tar.NewWriter(gzWriter)
		defer tarWriter.Close()

		info := mockFileInfo{
			name: "foo-db-name.mmdb",
			size: 1000,
		}
		header, err := tar.FileInfoHeader(info, info.Name())
		require.NoError(t, err)
		header.Name = "foo-db-name.mmdb"

		// Create a tar Header from the FileInfo data
		err = tarWriter.WriteHeader(header)
		require.NoError(t, err)

		bytesToWrite := 1000
		if try == 0 {
			// In the first try, we create a bad tar.gz file.
			// That has less than the size defined in the header.
			bytesToWrite = 100
		}

		for i := 0; i < bytesToWrite; i++ {
			_, err = tarWriter.Write([]byte("t"))
			require.NoError(t, err)
		}
		try++
	}))
	defer sv.Close()

	config := &Config{
		URL:               sv.URL,
		EditionIDs:        []string{"foo-db-name"},
		LockFile:          filepath.Join(tempDir, ".geoipupdate.lock"),
		Output:            true,
		Parallelism:       1,
		RetryFor:          5 * time.Minute,
		DatabaseDirectory: databaseDir,
	}

	logOutput := &bytes.Buffer{}

	writer, err := database.NewLocalFileWriter(
		config.DatabaseDirectory,
		config.PreserveFileTimes,
		config.Verbose,
	)
	require.NoError(t, err)

	u := &Updater{
		config: config,
		reader: database.NewHTTPReader(
			config.Proxy,
			config.URL,
			config.AccountID,
			config.LicenseKey,
			config.Verbose,
		),
		output: log.New(logOutput, "", 0),
		writer: writer,
	}

	ctx := context.Background()

	jobProcessor := internal.NewJobProcessor(ctx, 1)
	processFunc := func(ctx context.Context) error {
		_, err = u.downloadEdition(
			ctx,
			"foo-db-name",
			u.reader,
			u.writer,
		)

		return err
	}
	jobProcessor.Add(processFunc)

	err = jobProcessor.Run(ctx)
	require.NoError(t, err)

	assert.Empty(t, logOutput.String())
}

type mockReader struct {
	i      int
	result []database.ReadResult
}

func (mr *mockReader) Read(_ context.Context, _, _ string) (*database.ReadResult, error) {
	if mr.i >= len(mr.result) {
		return nil, errors.New("out of bounds")
	}
	res := mr.result[mr.i]
	mr.i++
	return &res, nil
}

type mockWriter struct {
	WriteFunc func(*database.ReadResult) error
}

func (w *mockWriter) Write(r *database.ReadResult) error {
	if w.WriteFunc != nil {
		return w.WriteFunc(r)
	}

	return nil
}
func (w mockWriter) GetHash(_ string) (string, error) { return "", nil }

func afterOrEqual(t1, t2 time.Time) bool {
	return t1.After(t2) || t1.Equal(t2)
}

type mockFileInfo struct {
	name string
	size int64
}

func (info mockFileInfo) Name() string {
	return info.name
}

func (info mockFileInfo) Size() int64 {
	return info.size
}

func (info mockFileInfo) IsDir() bool {
	return false
}

func (info mockFileInfo) Mode() os.FileMode {
	return 0
}

func (info mockFileInfo) ModTime() time.Time {
	return time.Now()
}

func (info mockFileInfo) Sys() any {
	return nil
}
