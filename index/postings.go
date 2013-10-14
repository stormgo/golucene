package index

import (
	"errors"
	"fmt"
	"github.com/balzaczyy/golucene/codec"
	"github.com/balzaczyy/golucene/store"
	"github.com/balzaczyy/golucene/util"
	"io"
	"log"
	"sort"
)

type FieldsProducer interface {
	Fields
	io.Closer
}

// BlockTreeTermsReader.java

const (
	BTT_OUTPUT_FLAGS_NUM_BITS = 2
	BTT_EXTENSION             = "tim"
	BTT_CODEC_NAME            = "BLOCK_TREE_TERMS_DICT"
	BTT_VERSION_START         = 0
	BTT_VERSION_APPEND_ONLY   = 1
	BTT_VERSION_CURRENT       = BTT_VERSION_APPEND_ONLY

	BTT_INDEX_EXTENSION           = "tip"
	BTT_INDEX_CODEC_NAME          = "BLOCK_TREE_TERMS_INDEX"
	BTT_INDEX_VERSION_START       = 0
	BTT_INDEX_VERSION_APPEND_ONLY = 1
	BTT_INDEX_VERSION_CURRENT     = BTT_INDEX_VERSION_APPEND_ONLY
)

/* A block-based terms index and dictionary that assigns
terms to variable length blocks according to how they
share prefixes. The terms index is a prefix trie
whose leaves are term blocks. The advantage of this
approach is that seekExact is often able to
determine a term cannot exist without doing any IO, and
intersection with Automata is very fast. NOte that this
terms dictionary has its own fixed terms index (ie, it
does not support a pluggable terms index
implementation).

NOTE: this terms dictionary does not support
index divisor when opening an IndexReader. Instead, you
can change the min/maxItemsPerBlock during indexing.

The data strucure used by this implementation is very
similar to a [burst trie]
(http://citeseer.ist.psu.edu/viewdoc/summary?doi=10.1.1.18.3499),
but with added logic to break up too-large blocks of all
terms sharing a given prefix into smaller ones.

Use CheckIndex with the -verbose
option to see summary statistics on the blocks in the
dictionary. */
type BlockTreeTermsReader struct {
	// Open input to the main terms dict file (_X.tib)
	in store.IndexInput
	// Reads the terms dict entries, to gather state to
	// produce DocsEnum on demand
	postingsReader PostingsReaderBase
	fields         map[string]FieldReader
	// File offset where the directory starts in the terms file.
	dirOffset int64
	// File offset where the directory starts in the index file.
	indexDirOffset int64
	segment        string
	version        int
}

func newBlockTreeTermsReader(dir store.Directory, fieldInfos FieldInfos, info SegmentInfo,
	postingsReader PostingsReaderBase, ctx store.IOContext,
	segmentSuffix string, indexDivisor int) (p FieldsProducer, err error) {
	log.Print("Initializing BlockTreeTermsReader...")
	fp := &BlockTreeTermsReader{
		postingsReader: postingsReader,
		fields:         make(map[string]FieldReader),
		segment:        info.name,
	}
	fp.in, err = dir.OpenInput(util.SegmentFileName(info.name, segmentSuffix, BTT_EXTENSION), ctx)
	if err != nil {
		return fp, err
	}

	success := false
	var indexIn store.IndexInput
	defer func() {
		if !success {
			log.Print("Failed to initialize BlockTreeTermsReader.")
			if err != nil {
				log.Print("DEBUG ", err)
			}
			// this.close() will close in:
			util.CloseWhileSuppressingError(indexIn, fp)
		}
	}()

	fp.version, err = fp.readHeader(fp.in)
	if err != nil {
		return fp, err
	}
	log.Printf("Version: %v", fp.version)

	if indexDivisor != -1 {
		indexIn, err = dir.OpenInput(util.SegmentFileName(info.name, segmentSuffix, BTT_INDEX_EXTENSION), ctx)
		if err != nil {
			return fp, err
		}

		indexVersion, err := fp.readIndexHeader(indexIn)
		if err != nil {
			return fp, err
		}
		log.Printf("Index version: %v", indexVersion)
		if int(indexVersion) != fp.version {
			return fp, errors.New(fmt.Sprintf("mixmatched version files: %v=%v,%v=%v", fp.in, fp.version, indexIn, indexVersion))
		}
	}

	// Have PostingsReader init itself
	postingsReader.Init(fp.in)

	// Read per-field details
	fp.seekDir(fp.in, fp.dirOffset)
	if indexDivisor != -1 {
		fp.seekDir(indexIn, fp.indexDirOffset)
	}

	numFields, err := fp.in.ReadVInt()
	if err != nil {
		return fp, err
	}
	log.Printf("Fields number: %v", numFields)
	if numFields < 0 {
		return fp, errors.New(fmt.Sprintf("invalid numFields: %v (resource=%v)", numFields, fp.in))
	}

	for i := int32(0); i < numFields; i++ {
		log.Printf("Next field...")
		field, err := fp.in.ReadVInt()
		if err != nil {
			return fp, err
		}
		log.Printf("Field: %v", field)

		numTerms, err := fp.in.ReadVLong()
		if err != nil {
			return fp, err
		}
		// assert numTerms >= 0
		log.Printf("Terms number: %v", numTerms)

		numBytes, err := fp.in.ReadVInt()
		if err != nil {
			return fp, err
		}
		log.Printf("Bytes number: %v", numBytes)

		rootCode := make([]byte, numBytes)
		err = fp.in.ReadBytes(rootCode)
		if err != nil {
			return fp, err
		}
		fieldInfo := fieldInfos.byNumber[field]
		// assert fieldInfo != nil
		var sumTotalTermFreq int64
		if fieldInfo.indexOptions == INDEX_OPT_DOCS_ONLY {
			sumTotalTermFreq = -1
		} else {
			sumTotalTermFreq, err = fp.in.ReadVLong()
			if err != nil {
				return fp, err
			}
		}
		sumDocFreq, err := fp.in.ReadVLong()
		if err != nil {
			return fp, err
		}
		docCount, err := fp.in.ReadVInt()
		if err != nil {
			return fp, err
		}
		log.Printf("DocCount: %v", docCount)
		if docCount < 0 || docCount > info.docCount { // #docs with field must be <= #docs
			return fp, errors.New(fmt.Sprintf(
				"invalid docCount: %v maxDoc: %v (resource=%v)",
				docCount, info.docCount, fp.in))
		}
		if sumDocFreq < int64(docCount) { // #postings must be >= #docs with field
			return fp, errors.New(fmt.Sprintf(
				"invalid sumDocFreq: %v docCount: %v (resource=%v)",
				sumDocFreq, docCount, fp.in))
		}
		if sumTotalTermFreq != -1 && sumTotalTermFreq < sumDocFreq { // #positions must be >= #postings
			return fp, errors.New(fmt.Sprintf(
				"invalid sumTotalTermFreq: %v sumDocFreq: %v (resource=%v)",
				sumTotalTermFreq, sumDocFreq, fp.in))
		}

		var indexStartFP int64
		if indexDivisor != -1 {
			indexStartFP, err = indexIn.ReadVLong()
			if err != nil {
				return fp, err
			}
		}
		log.Printf("indexStartFP: %v", indexStartFP)
		if _, ok := fp.fields[fieldInfo.name]; ok {
			return fp, errors.New(fmt.Sprintf(
				"duplicate field: %v (resource=%v)", fieldInfo.name, fp.in))
		}
		fp.fields[fieldInfo.name], err = newFieldReader(
			fieldInfo, numTerms, rootCode, sumTotalTermFreq,
			sumDocFreq, docCount, indexStartFP, indexIn)
		if err != nil {
			return fp, err
		}
		log.Print("DEBUG field processed.")
	}

	if indexDivisor != -1 {
		err = indexIn.Close()
		if err != nil {
			return fp, err
		}
	}

	success = true

	return fp, nil
}

func asInt(n int32, err error) (n2 int, err2 error) {
	return int(n), err
}

func (r *BlockTreeTermsReader) readHeader(input store.IndexInput) (version int, err error) {
	version, err = asInt(codec.CheckHeader(input, BTT_CODEC_NAME, BTT_VERSION_START, BTT_VERSION_CURRENT))
	if err != nil {
		return int(version), err
	}
	if version < BTT_VERSION_APPEND_ONLY {
		r.dirOffset, err = input.ReadLong()
		if err != nil {
			return int(version), err
		}
	}
	return int(version), nil
}

func (r *BlockTreeTermsReader) readIndexHeader(input store.IndexInput) (version int, err error) {
	version, err = asInt(codec.CheckHeader(input, BTT_INDEX_CODEC_NAME, BTT_INDEX_VERSION_START, BTT_INDEX_VERSION_CURRENT))
	if err != nil {
		return version, err
	}
	if version < BTT_INDEX_VERSION_APPEND_ONLY {
		r.indexDirOffset, err = input.ReadLong()
		if err != nil {
			return version, err
		}
	}
	return version, nil
}

func (r *BlockTreeTermsReader) seekDir(input store.IndexInput, dirOffset int64) (err error) {
	log.Printf("Seeking to: %v", dirOffset)
	if r.version >= BTT_INDEX_VERSION_APPEND_ONLY {
		input.Seek(input.Length() - 8)
		if dirOffset, err = input.ReadLong(); err != nil {
			return err
		}
	}
	input.Seek(dirOffset)
	return nil
}

func (r *BlockTreeTermsReader) Terms(field string) Terms {
	ans := r.fields[field]
	return &ans
}

func (r *BlockTreeTermsReader) Close() error {
	defer func() {
		// Clear so refs to terms index is GCable even if
		// app hangs onto us:
		r.fields = make(map[string]FieldReader)
	}()
	return util.Close(r.in, r.postingsReader)
}

type FieldReader struct {
	owner            *BlockTreeTermsReader // inner class
	numTerms         int64
	fieldInfo        FieldInfo
	sumTotalTermFreq int64
	sumDocFreq       int64
	docCount         int32
	indexStartFP     int64
	rootBlockFP      int64
	rootCode         []byte
	index            *util.FST
}

func newFieldReader(fieldInfo FieldInfo, numTerms int64, rootCode []byte,
	sumTotalTermFreq, sumDocFreq int64, docCount int32, indexStartFP int64,
	indexIn store.IndexInput) (r FieldReader, err error) {
	log.Print("Initializing FieldReader...")
	// assert numTerms > 0
	r = FieldReader{
		fieldInfo:        fieldInfo,
		numTerms:         numTerms,
		sumTotalTermFreq: sumTotalTermFreq,
		sumDocFreq:       sumDocFreq,
		docCount:         docCount,
		indexStartFP:     indexStartFP,
		rootCode:         rootCode}

	in := store.NewByteArrayDataInput(rootCode)
	n, err := in.ReadVLong()
	if err != nil {
		return r, err
	}
	r.rootBlockFP = int64(uint64(n) >> BTT_OUTPUT_FLAGS_NUM_BITS)

	if indexIn != nil {
		clone := indexIn.Clone()
		clone.Seek(indexStartFP)
		r.index, err = util.LoadFST(clone, util.ByteSequenceOutputsSingleton())
	}

	return r, err
}

func (r *FieldReader) Iterator(reuse TermsEnum) TermsEnum {
	return newSegmentTermsEnum(r)
}

func (r *FieldReader) SumTotalTermFreq() int64 {
	return r.sumTotalTermFreq
}

func (r *FieldReader) SumDocFreq() int64 {
	return r.sumDocFreq
}

func (r *FieldReader) DocCount() int {
	return int(r.docCount)
}

// BlockTreeTermsReader.java/SegmentTermsEnum
// Iterates through terms in this field
type SegmentTermsEnum struct {
	*TermsEnumImpl
	owner *FieldReader

	in store.IndexInput

	stack        []segmentTermsEnumFrame
	staticFrame  segmentTermsEnumFrame
	currentFrame segmentTermsEnumFrame
	termExists   bool

	targetBeforeCurrentLength int

	// What prefix of the current term was present in the index:
	scratchReader *store.ByteArrayDataInput

	// assert only:
	eof bool

	term      []byte
	fstReader util.BytesReader

	arcs []util.Arc
}

func newSegmentTermsEnum(r *FieldReader) *SegmentTermsEnum {
	ans := &SegmentTermsEnum{
		owner:         r,
		scratchReader: store.NewByteArrayDataInput(nil),
		arcs:          make([]util.Arc, 1),
	}
	ans.TermsEnumImpl = newTermsEnumImpl(ans)
	return ans
}

func (e *SegmentTermsEnum) Comparator() sort.Interface {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) SeekExact(target []byte) bool {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) SeekCeil(text []byte) SeekStatus {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) Next() (buf []byte, err error) {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) Term() []byte {
	if e.eof {
		panic("assertion error")
	}
	return e.term
}

func (e *SegmentTermsEnum) DocFreq() int {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) TotalTermFreq() int64 {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) DocsByFlags(skipDocs util.Bits, reuse DocsEnum, flags int) DocsEnum {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) DocsAndPositionsByFlags(skipDocs util.Bits, reuse DocsAndPositionsEnum, flags int) DocsAndPositionsEnum {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) SeekExactFromLast(target []byte, otherState TermState) error {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) TermState() TermState {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) SeekExactByPosition(ord int64) error {
	panic("not implemented yet")
}

func (e *SegmentTermsEnum) Ord() int64 {
	panic("not supported!")
}

type segmentTermsEnumFrame struct {
	// internal data structure
	owner *SegmentTermsEnum

	// Our index in stack[]:
	ord int

	hasTerms     bool
	hasTermsOrig bool
	isFloor      bool

	arc util.Arc

	// File pointer where this block was loaded from
	fp     int64
	fpOrig int64
	fpEnd  int64

	suffixBytes    []byte
	suffixesReader store.ByteArrayDataInput

	statBytes   []byte
	statsReader store.ByteArrayDataInput

	floorData       []byte
	floorDataReader store.ByteArrayDataInput

	// Length of prefix shared by all terms in this block
	prefix int

	// Number of entries (term or sub-block) in this block
	entCount int

	// Which term we will next read, or -1 if the block
	// isn't loaded yet
	nextEnt int

	// True if this block is either not a floor block,
	// or, it's the last sub-block of a floor block
	isLastInFloor bool

	// True if all entries are terms
	isLeafBlock bool

	lastSubFP int64

	nextFloorLabel       int
	numFollowFloorBlocks int

	// Next term to decode metaData; we decode metaData
	// lazily so that scanning to find the matching term is
	// fast and only if you find a match and app wants the
	// stats or docs/positions enums, will we decode the
	// metaData
	metaDataUpto int

	state *BlockTermState
}

func newFrame(owner *SegmentTermsEnum, ord int) *segmentTermsEnumFrame {
	f := &segmentTermsEnumFrame{
		owner:       owner,
		suffixBytes: make([]byte, 128),
		statBytes:   make([]byte, 64),
		floorData:   make([]byte, 32),
		ord:         ord,
	}
	f.state = owner.owner.owner.postingsReader.NewTermState()
	f.state.totalTermFreq = -1
	return f
}
