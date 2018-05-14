package renter

import (
	"github.com/lukechampine/us/hostdb"
	"github.com/lukechampine/us/renter/proto"

	"github.com/NebulousLabs/Sia/crypto"
	"github.com/NebulousLabs/Sia/types"
	"github.com/NebulousLabs/fastrand"
	"github.com/pkg/errors"
)

// A SectorBuilder facilitates the construction of sectors for later upload.
// SectorBuilders are particularly useful when packing data from multiple
// sources into a single sector. The zero value for a SectorBuilder is an
// empty sector.
type SectorBuilder struct {
	sector    [proto.SectorSize]byte
	sectorLen int
	slices    []SectorSlice
}

// Reset resets the SectorBuilder to its initial state.
//
// Reset does not allocate a new sector buffer; since Finish returns a pointer
// to the buffer, this pointer should not be retained after Reset is called.
func (sb *SectorBuilder) Reset() {
	sb.sectorLen = 0
	sb.slices = nil // can't reuse capacity; Slices shares memory
}

// Append appends data to the sector being constructed, encrypting it with the
// given key and chunkIndex. The data is also padded with random bytes to the
// nearest multiple of proto.SegmentSize, which is required by the encryption
// scheme. Each call to Append creates a SectorSlice that is accessible via
// the Slices method. This SectorSlice reflects the length and checksum of the
// original (unpadded, unencrypted) data.
//
// Append panics if len(data) > sb.Remaining().
func (sb *SectorBuilder) Append(data []byte, key EncryptionKey, chunkIndex int64) {
	// pad the data to a multiple of SegmentSize, which is required
	// by the encryption scheme
	var padding int
	if mod := len(data) % proto.SegmentSize; mod != 0 {
		padding = proto.SegmentSize - mod
	}

	if sb.sectorLen+len(data)+padding > proto.SectorSize {
		// TODO: make this nicer?
		panic("data exceeds sector size")
	}

	// copy the data into the sector, adding random padding if necessary
	sectorSlice := sb.sector[sb.sectorLen:][:len(data)+padding]
	copy(sectorSlice, data)
	fastrand.Read(sectorSlice[len(data):])

	// encrypt the data+padding in place
	//
	// NOTE: to avoid reusing the same segment index for multiple encryptions,
	// we use chunkIndex * SegmentsPerSector as the starting index. This is
	// slightly wasteful, but the space is large enough that we can afford it.
	// (In the worst case, a single byte of data is stored per chunkIndex; we
	// can then store up to (2^64/SegmentsPerSector) = 2^48 bytes, or about
	// 280 TB. In the average case, all but the final sector will be "full",
	// so the waste is negligible.)
	startIndex := uint64(chunkIndex * proto.SegmentsPerSector)
	key.EncryptSegments(sectorSlice, sectorSlice, startIndex)

	// update sectorLen and record the new slice
	sb.slices = append(sb.slices, SectorSlice{
		Offset:   uint32(sb.sectorLen),
		Length:   uint32(len(data)),
		Checksum: crypto.HashBytes(data),
	})
	sb.sectorLen += len(sectorSlice)
}

// Len returns the number of bytes appended to the sector. Note that, due to
// padding, it is not generally true that Len equals the sum of slices passed
// to Append.
func (sb *SectorBuilder) Len() int {
	return sb.sectorLen
}

// Remaining returns the number of bytes remaining in the sector. It is
// equivalent to proto.SectorSize - sb.Len().
func (sb *SectorBuilder) Remaining() int {
	return len(sb.sector) - sb.sectorLen
}

// Finish fills the remaining capacity of the sector with random bytes and
// returns it. The MerkleRoot field of each SectorSlice tracked by sb is set
// to Merkle root of the resulting sector.
//
// After calling Finish, Len returns proto.SectorSize and Remaining returns 0;
// no more data can be appended until Reset is called.
func (sb *SectorBuilder) Finish() *[proto.SectorSize]byte {
	fastrand.Read(sb.sector[sb.sectorLen:])
	sb.sectorLen = len(sb.sector)

	// set Merkle root of each slice
	sectorRoot := proto.SectorMerkleRoot(&sb.sector)
	for i := range sb.slices {
		sb.slices[i].MerkleRoot = sectorRoot
	}

	return &sb.sector
}

// Slices returns the SectorSlices present in the sector. One SectorSlice is
// returned for each call to Append since the last call to Reset. Slices
// should only be called after calling Finish; otherwise the MerkleRoot field
// of each SectorSlice will be unset.
func (sb *SectorBuilder) Slices() []SectorSlice {
	return sb.slices
}

// A ShardUploader wraps a proto.Uploader to provide SectorSlice-based data
// storage, transparently encrypting and checksumming all data before
// transferring it to the host.
type ShardUploader struct {
	Uploader *proto.Uploader
	Shard    *Shard
	Key      EncryptionKey
	sb       SectorBuilder
}

// EncryptAndUpload uploads the data associated with chunkIndex, creating a
// SectorSlice. The data is encrypted and padded to proto.SectorSize before it
// is uploaded. The resulting SectorSlice is written to u.Shard.
func (u *ShardUploader) EncryptAndUpload(data []byte, chunkIndex int64) (SectorSlice, error) {
	if len(data) > proto.SectorSize {
		return SectorSlice{}, errors.New("data exceeds sector size")
	}

	u.sb.Reset()
	u.sb.Append(data, u.Key, chunkIndex)
	_, err := u.Uploader.Upload(u.sb.Finish())
	if err != nil {
		return SectorSlice{}, err
	}
	ss := u.sb.Slices()[0]

	// add slice to shard
	err = u.Shard.WriteSlice(ss, chunkIndex)
	if err != nil {
		return SectorSlice{}, errors.Wrap(err, "could not write to shard file")
	}

	return ss, nil
}

// HostKey returns the public key of the host.
func (u *ShardUploader) HostKey() hostdb.HostPublicKey {
	return u.Uploader.HostKey()
}

// Close closes the connection to the host and the Shard file.
func (u *ShardUploader) Close() error {
	u.Uploader.Close()
	u.Shard.Close()
	return nil
}

// NewShardUploader connects to a host and returns a ShardUploader capable of
// uploading m's data and writing to one of m's Shard files.
func NewShardUploader(m *MetaFile, hostIndex int, contract *Contract, scan ScanFn, currentHeight types.BlockHeight) (*ShardUploader, error) {
	hostKey := contract.HostKey()
	// open shard
	sf, err := OpenShard(m.ShardPath(hostKey))
	if err != nil {
		return nil, errors.Wrapf(err, "%v: could not load shard file", hostKey.ShortKey())
	}
	// get host entry
	host, err := scan(contract.HostKey())
	if err != nil {
		sf.Close()
		return nil, errors.Wrapf(err, "%v: could not scan host", hostKey.ShortKey())
	}
	// create uploader
	u, err := proto.NewUploader(host, contract, currentHeight)
	if err != nil {
		sf.Close()
		return nil, errors.Wrapf(err, "%v: could not initiate upload protocol with host", hostKey.ShortKey())
	}
	return &ShardUploader{
		Uploader: u,
		Shard:    sf,
		Key:      m.EncryptionKey(hostIndex),
	}, nil
}
