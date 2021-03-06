package retrying_test

import (
	"testing"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/blobtesting"
	"github.com/kopia/kopia/internal/gather"
	"github.com/kopia/kopia/internal/testlogging"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/retrying"
)

func TestRetrying(t *testing.T) {
	ctx := testlogging.Context(t)

	someError := errors.New("some error")
	ms := blobtesting.NewMapStorage(blobtesting.DataMap{}, nil, nil)
	fs := &blobtesting.FaultyStorage{
		Base: ms,
		Faults: map[string][]*blobtesting.Fault{
			"PutBlob": {
				{Err: someError},
			},
			"GetBlob": {
				{Err: someError},
			},
			"GetMetadata": {
				{Err: someError},
			},
			"DeleteBlob": {
				{Err: someError},
			},
			"SetTime": {
				{Err: someError},
			},
		},
	}

	rs := retrying.NewWrapper(fs)
	blobID := blob.ID("deadcafe")
	blobID2 := blob.ID("deadcafe2")

	must(t, rs.PutBlob(ctx, blobID, gather.FromSlice([]byte{1, 2, 3})))

	must(t, rs.PutBlob(ctx, blobID2, gather.FromSlice([]byte{1, 2, 3, 4})))

	must(t, rs.SetTime(ctx, blobID, time.Now()))

	_, err := rs.GetBlob(ctx, blobID, 0, -1)
	must(t, err)

	_, err = rs.GetMetadata(ctx, blobID)
	must(t, err)

	must(t, rs.DeleteBlob(ctx, blobID))

	if _, err = rs.GetBlob(ctx, blobID, 0, -1); !errors.Is(err, blob.ErrBlobNotFound) {
		t.Fatalf("unexpected error: %v", err)
	}

	fs.VerifyAllFaultsExercised(t)
}

func must(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}
