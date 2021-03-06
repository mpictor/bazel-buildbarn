package blobstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"hash"
	"io"
	"log"

	"github.com/EdSchouten/bazel-buildbarn/pkg/util"
	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// extractDigest validates the format of fields in a Digest object and returns them.
func extractDigest(digest *remoteexecution.Digest) ([]byte, int64, util.DigestFormat, error) {
	digestFormat, err := util.DigestFormatFromLength(len(digest.Hash))
	if err != nil {
		return nil, 0, nil, err
	}

	// hex.DecodeString() also permits uppercase characters, which
	// would lead to duplicate representations. Reject those
	// explicitly prior to calling hex.DecodeString().
	for _, c := range digest.Hash {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return nil, 0, nil, status.Errorf(codes.InvalidArgument, "Non-hexadecimal character in digest hash: %#U", c)
		}
	}
	checksum, err := hex.DecodeString(digest.Hash)
	if err != nil {
		log.Fatal("Failed to decode digest hash, even though its contents have already been validated")
	}

	if digest.SizeBytes < 0 {
		return nil, 0, nil, status.Errorf(codes.InvalidArgument, "Invalid digest size: %d bytes", digest.SizeBytes)
	}
	return checksum, digest.SizeBytes, digestFormat, nil
}

type merkleBlobAccess struct {
	blobAccess BlobAccess
}

// NewMerkleBlobAccess creates an adapter that validates that blobs read
// from and written to storage correspond with the digest that is used
// for identification. It ensures that the size and the SHA-256 based
// checksum match. This is used to ensure clients cannot corrupt the CAS
// and that if corruption were to occur, use of corrupted data is prevented.
func NewMerkleBlobAccess(blobAccess BlobAccess) BlobAccess {
	return &merkleBlobAccess{
		blobAccess: blobAccess,
	}
}

func (ba *merkleBlobAccess) Get(ctx context.Context, instance string, digest *remoteexecution.Digest) io.ReadCloser {
	checksum, size, digestFormat, err := extractDigest(digest)
	if err != nil {
		return util.NewErrorReader(err)
	}
	return &checksumValidatingReader{
		ReadCloser:       ba.blobAccess.Get(ctx, instance, digest),
		expectedChecksum: checksum,
		partialChecksum:  digestFormat(),
		sizeLeft:         size,
		invalidator: func() {
			// Trigger blob deletion in case we detect data
			// corruption. This will cause future calls to
			// FindMissing() to indicate absence, causing clients to
			// re-upload them and/or build actions to be retried.
			if err := ba.blobAccess.Delete(ctx, instance, digest); err == nil {
				log.Printf("Successfully deleted corrupted blob %s", digest)
			} else {
				log.Printf("Failed to delete corrupted blob %s: %s", digest, err)
			}
		},
		errorCode: codes.Internal,
	}
}

func (ba *merkleBlobAccess) Put(ctx context.Context, instance string, digest *remoteexecution.Digest, sizeBytes int64, r io.ReadCloser) error {
	checksum, digestSizeBytes, digestFormat, err := extractDigest(digest)
	if err != nil {
		r.Close()
		return err
	}
	if digestSizeBytes != sizeBytes {
		log.Fatal("Called into CAS to store non-CAS object")
	}
	return ba.blobAccess.Put(ctx, instance, digest, digestSizeBytes, &checksumValidatingReader{
		ReadCloser:       r,
		expectedChecksum: checksum,
		partialChecksum:  digestFormat(),
		sizeLeft:         digestSizeBytes,
		invalidator:      func() {},
		errorCode:        codes.InvalidArgument,
	})
}

func (ba *merkleBlobAccess) Delete(ctx context.Context, instance string, digest *remoteexecution.Digest) error {
	_, _, _, err := extractDigest(digest)
	if err != nil {
		return err
	}
	return ba.blobAccess.Delete(ctx, instance, digest)
}

func (ba *merkleBlobAccess) FindMissing(ctx context.Context, instance string, digests []*remoteexecution.Digest) ([]*remoteexecution.Digest, error) {
	for _, digest := range digests {
		_, _, _, err := extractDigest(digest)
		if err != nil {
			return nil, err
		}
	}
	return ba.blobAccess.FindMissing(ctx, instance, digests)
}

type checksumValidatingReader struct {
	io.ReadCloser

	expectedChecksum []byte
	partialChecksum  hash.Hash
	sizeLeft         int64

	// Called whenever size/checksum inconsistencies are detected.
	invalidator func()
	errorCode   codes.Code
}

func (r *checksumValidatingReader) Read(p []byte) (int, error) {
	n, err := io.TeeReader(r.ReadCloser, r.partialChecksum).Read(p)
	nLen := int64(n)
	if nLen > r.sizeLeft {
		r.invalidator()
		return 0, status.Error(r.errorCode, "Blob is longer than expected")
	}
	r.sizeLeft -= nLen

	if err == io.EOF {
		if r.sizeLeft != 0 {
			r.invalidator()
			return 0, status.Errorf(r.errorCode, "Blob is %d bytes shorter than expected", r.sizeLeft)
		}

		actualChecksum := r.partialChecksum.Sum(nil)
		if bytes.Compare(actualChecksum, r.expectedChecksum) != 0 {
			r.invalidator()
			return 0, status.Errorf(
				r.errorCode,
				"Checksum of blob is %s, while %s was expected",
				hex.EncodeToString(actualChecksum),
				hex.EncodeToString(r.expectedChecksum))
		}
	}
	return n, err
}
