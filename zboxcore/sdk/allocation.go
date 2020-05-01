package sdk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/0chain/gosdk/zboxcore/fileref"

	"github.com/0chain/gosdk/core/common"
	"github.com/0chain/gosdk/zboxcore/blockchain"
	. "github.com/0chain/gosdk/zboxcore/logger"
	"github.com/0chain/gosdk/zboxcore/marker"
	"github.com/0chain/gosdk/zboxcore/zboxutil"
)

var (
	noBLOBBERS     = errors.New("No Blobbers set in this allocation")
	notInitialized = common.NewError("sdk_not_initialized", "Please call InitStorageSDK Init and use GetAllocation to get the allocation object")
	underRepair    = common.NewError("allocaton_under_repair", "Allocation is under repair, Please try again later")
)

type ConsolidatedFileMeta struct {
	Name           string
	Type           string
	Path           string
	LookupHash     string
	Hash           string
	MimeType       string
	Size           int64
	EncryptedKey   string
	CommitMetaTxns []fileref.CommitMetaTxn
}

type AllocationStats struct {
	UsedSize                  int64  `json:"used_size"`
	NumWrites                 int64  `json:"num_of_writes"`
	NumReads                  int64  `json:"num_of_reads"`
	TotalChallenges           int64  `json:"total_challenges"`
	OpenChallenges            int64  `json:"num_open_challenges"`
	SuccessChallenges         int64  `json:"num_success_challenges"`
	FailedChallenges          int64  `json:"num_failed_challenges"`
	LastestClosedChallengeTxn string `json:"latest_closed_challenge"`
}

// PriceRange represents a price range allowed by user to filter blobbers.
type PriceRange struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

// IsValid price range.
func (pr *PriceRange) IsValid() bool {
	return 0 <= pr.Min && pr.Min <= pr.Max
}

// Terms represents Blobber terms. A Blobber can update its terms,
// but any existing offer will use terms of offer signing time.
type Terms struct {
	ReadPrice               common.Balance `json:"read_price"`  // tokens / GB
	WritePrice              common.Balance `json:"write_price"` // tokens / GB
	MinLockDemand           float64        `json:"min_lock_demand"`
	MaxOfferDuration        time.Duration  `json:"max_offer_duration"`
	ChallengeCompletionTime time.Duration  `json:"challenge_completion_time"`
}

type BlobberAllocation struct {
	BlobberID       string         `json:"blobber_id"`
	Size            int64          `json:"size"`
	Terms           Terms          `json:"terms"`
	MinLockDemand   common.Balance `json:"min_lock_demand"`
	Spent           common.Balance `json:"spent"`
	Penalty         common.Balance `json:"penalty"`
	ReadReward      common.Balance `json:"read_reward"`
	Returned        common.Balance `json:"returned"`
	ChallengeReward common.Balance `json:"challenge_reward"`
	FinalReward     common.Balance `json:"final_reward"`
}

type Allocation struct {
	ID             string                    `json:"id"`
	Tx             string                    `json:"tx"`
	DataShards     int                       `json:"data_shards"`
	ParityShards   int                       `json:"parity_shards"`
	Size           int64                     `json:"size"`
	Expiration     int64                     `json:"expiration_date"`
	Owner          string                    `json:"owner_id"`
	OwnerPublicKey string                    `json:"owner_public_key"`
	Payer          string                    `json:"payer_id"`
	Blobbers       []*blockchain.StorageNode `json:"blobbers"`
	Stats          *AllocationStats          `json:"stats"`

	// BlobberDetails contains real terms used for the allocation.
	// If the allocation has updated, then terms calculated using
	// weighted average values.
	BlobberDetails []*BlobberAllocation `json:"blobber_details"`

	// ReadPriceRange is requested reading prices range.
	ReadPriceRange PriceRange `json:"read_price_range"`
	// WritePriceRange is requested writing prices range.
	WritePriceRange PriceRange `json:"write_price_range"`

	ChallengeCompletionTime time.Duration    `json:"challenge_completion_time"`
	StartTime               common.Timestamp `json:"start_time"`
	Finalized               bool             `json:"finalized,omitempty"`
	Canceled                bool             `json:"canceled,omitempty"`
	MovedToChallenge        common.Balance   `json:"moved_to_challenge,omitempty"`
	MovedBack               common.Balance   `json:"moved_back,omitempty"`
	MovedToValidators       common.Balance   `json:"moved_to_validators,omitempty"`

	numBlockDownloads   int
	uploadChan          chan *UploadRequest
	downloadChan        chan *DownloadRequest
	ctx                 context.Context
	ctxCancelF          context.CancelFunc
	mutex               *sync.Mutex
	uploadProgressMap   map[string]*UploadRequest
	downloadProgressMap map[string]*DownloadRequest
	initialized         bool
	underRepair         bool
}

func (a *Allocation) UnderRepair() bool {
	return a.underRepair
}

func (a *Allocation) GetStats() *AllocationStats {
	return a.Stats
}

func (a *Allocation) UpdateRepairStatus(value bool) {
	a.underRepair = value
}

func (a *Allocation) InitAllocation() {
	// if a.uploadChan != nil {
	// 	close(a.uploadChan)
	// }
	// if a.downloadChan != nil {
	// 	close(a.downloadChan)
	// }
	// if a.ctx != nil {
	// 	a.ctx.Done()
	// }
	// for _, v := range a.downloadProgressMap {
	// 	v.isDownloadCanceled = true
	// }
	a.uploadChan = make(chan *UploadRequest, 10)
	a.downloadChan = make(chan *DownloadRequest, 10)
	a.ctx, a.ctxCancelF = context.WithCancel(context.Background())
	a.uploadProgressMap = make(map[string]*UploadRequest)
	a.downloadProgressMap = make(map[string]*DownloadRequest)
	a.mutex = &sync.Mutex{}
	a.startWorker(a.ctx)
	InitCommitWorker(a.Blobbers)
	InitBlockDownloader(a.Blobbers)
	a.initialized = true
}

func (a *Allocation) isInitialized() bool {
	return a.initialized && sdkInitialized
}

func (a *Allocation) startWorker(ctx context.Context) {
	go a.dispatchWork(ctx)
}

func (a *Allocation) dispatchWork(ctx context.Context) {
	for true {
		select {
		case <-ctx.Done():
			Logger.Info("Upload cancelled by the parent")
			return
		case uploadReq := <-a.uploadChan:

			Logger.Info(fmt.Sprintf("received a upload request for %v %v\n", uploadReq.filepath, uploadReq.remotefilepath))
			go uploadReq.processUpload(ctx, a)
		case downloadReq := <-a.downloadChan:

			Logger.Info(fmt.Sprintf("received a download request for %v\n", downloadReq.remotefilepath))
			go downloadReq.processDownload(ctx, a)
		}
	}
}

func (a *Allocation) UpdateFile(localpath string, remotepath string, status StatusCallback) error {
	return a.uploadOrUpdateFile(localpath, remotepath, status, true, "", false, false)
}

func (a *Allocation) UploadFile(localpath string, remotepath string, status StatusCallback) error {
	return a.uploadOrUpdateFile(localpath, remotepath, status, false, "", false, false)
}

func (a *Allocation) RepairFile(localpath string, remotepath string, status StatusCallback) error {
	return a.uploadOrUpdateFile(localpath, remotepath, status, false, "", false, true)
}

func (a *Allocation) UpdateFileWithThumbnail(localpath string, remotepath string, thumbnailpath string, status StatusCallback) error {
	return a.uploadOrUpdateFile(localpath, remotepath, status, true, thumbnailpath, false, false)
}

func (a *Allocation) UploadFileWithThumbnail(localpath string, remotepath string, thumbnailpath string, status StatusCallback) error {
	return a.uploadOrUpdateFile(localpath, remotepath, status, false, thumbnailpath, false, false)
}

func (a *Allocation) EncryptAndUpdateFile(localpath string, remotepath string, status StatusCallback) error {
	return a.uploadOrUpdateFile(localpath, remotepath, status, true, "", true, false)
}

func (a *Allocation) EncryptAndUploadFile(localpath string, remotepath string, status StatusCallback) error {
	return a.uploadOrUpdateFile(localpath, remotepath, status, false, "", true, false)
}

func (a *Allocation) EncryptAndUpdateFileWithThumbnail(localpath string, remotepath string, thumbnailpath string, status StatusCallback) error {
	return a.uploadOrUpdateFile(localpath, remotepath, status, true, thumbnailpath, true, false)
}

func (a *Allocation) EncryptAndUploadFileWithThumbnail(localpath string, remotepath string, thumbnailpath string, status StatusCallback) error {
	return a.uploadOrUpdateFile(localpath, remotepath, status, false, thumbnailpath, true, false)
}

func (a *Allocation) uploadOrUpdateFile(localpath string, remotepath string, status StatusCallback, isUpdate bool, thumbnailpath string, encryption bool, isRepair bool) error {
	if !a.isInitialized() {
		return notInitialized
	}
	if a.UnderRepair() {
		return underRepair
	}

	fileInfo, err := os.Stat(localpath)
	if err != nil {
		return fmt.Errorf("Local file error: %s", err.Error())
	}
	thumbnailSize := int64(0)
	if len(thumbnailpath) > 0 {
		fileInfo, err := os.Stat(thumbnailpath)
		if err != nil {
			thumbnailSize = 0
			thumbnailpath = ""
		} else {
			thumbnailSize = fileInfo.Size()
		}

	}

	remotepath = zboxutil.RemoteClean(remotepath)
	isabs := zboxutil.IsRemoteAbs(remotepath)
	if !isabs {
		return common.NewError("invalid_path", "Path should be valid and absolute")
	}
	remotepath = zboxutil.GetFullRemotePath(localpath, remotepath)

	var fileName string
	_, fileName = filepath.Split(remotepath)
	uploadReq := &UploadRequest{}
	uploadReq.remotefilepath = remotepath
	uploadReq.thumbnailpath = thumbnailpath
	uploadReq.filepath = localpath
	uploadReq.filemeta = &UploadFileMeta{}
	uploadReq.filemeta.Name = fileName
	uploadReq.filemeta.Size = fileInfo.Size()
	uploadReq.filemeta.Path = remotepath
	uploadReq.filemeta.ThumbnailSize = thumbnailSize
	uploadReq.remaining = uploadReq.filemeta.Size
	uploadReq.thumbRemaining = uploadReq.filemeta.ThumbnailSize
	uploadReq.isRepair = false
	uploadReq.isUpdate = isUpdate
	uploadReq.isRepair = isRepair
	uploadReq.connectionID = zboxutil.NewConnectionId()
	uploadReq.statusCallback = status
	uploadReq.datashards = a.DataShards
	uploadReq.parityshards = a.ParityShards
	uploadReq.uploadMask = uint32((1 << uint32(len(a.Blobbers))) - 1)
	uploadReq.consensusThresh = (float32(a.DataShards) * 100) / float32(a.DataShards+a.ParityShards)
	uploadReq.fullconsensus = float32(a.DataShards + a.ParityShards)
	uploadReq.isEncrypted = encryption
	uploadReq.completedCallback = func(filepath string) {
		a.mutex.Lock()
		defer a.mutex.Unlock()
		delete(a.uploadProgressMap, filepath)
	}

	if uploadReq.isRepair {
		listReq := &ListRequest{}
		listReq.allocationID = a.ID
		listReq.allocationTx = a.Tx
		listReq.blobbers = a.Blobbers
		listReq.consensusThresh = (float32(a.DataShards) * 100) / float32(a.DataShards+a.ParityShards)
		listReq.fullconsensus = float32(a.DataShards + a.ParityShards)
		listReq.ctx = a.ctx
		listReq.remotefilepath = remotepath
		found, fileRef, _ := listReq.getFileConsensusFromBlobbers()
		if fileRef == nil {
			return fmt.Errorf("File not found for the given remotepath")
		}
		if found == uploadReq.uploadMask {
			return fmt.Errorf("No repair required")
		}

		uploadReq.uploadMask = (^found & uploadReq.uploadMask)
		uploadReq.fullconsensus = uploadReq.fullconsensus - float32(bits.TrailingZeros32(uploadReq.uploadMask))
		a.UpdateRepairStatus(true)
	}

	go func() {
		a.uploadChan <- uploadReq
		a.mutex.Lock()
		defer a.mutex.Unlock()
		a.uploadProgressMap[localpath] = uploadReq
	}()
	return nil
}

func (a *Allocation) DownloadFile(localPath string, remotePath string, status StatusCallback) error {
	return a.downloadFile(localPath, remotePath, DOWNLOAD_CONTENT_FULL, status)
}

func (a *Allocation) DownloadThumbnail(localPath string, remotePath string, status StatusCallback) error {
	return a.downloadFile(localPath, remotePath, DOWNLOAD_CONTENT_THUMB, status)
}

func (a *Allocation) downloadFile(localPath string, remotePath string, contentMode string, status StatusCallback) error {
	if !a.isInitialized() {
		return notInitialized
	}
	if a.UnderRepair() {
		return underRepair
	}
	if stat, err := os.Stat(localPath); err == nil {
		if !stat.IsDir() {
			return fmt.Errorf("Local path is not a directory '%s'", localPath)
		}
		localPath = strings.TrimRight(localPath, "/")
		_, rFile := filepath.Split(remotePath)
		localPath = fmt.Sprintf("%s/%s", localPath, rFile)
		if _, err := os.Stat(localPath); err == nil {
			return fmt.Errorf("Local file already exists '%s'", localPath)
		}
	}
	lPath, _ := filepath.Split(localPath)
	os.MkdirAll(lPath, os.ModePerm)

	if len(a.Blobbers) <= 1 {
		return noBLOBBERS
	}

	downloadReq := &DownloadRequest{}
	downloadReq.allocationID = a.ID
	downloadReq.allocationTx = a.Tx
	downloadReq.ctx, _ = context.WithCancel(a.ctx)
	downloadReq.localpath = localPath
	downloadReq.remotefilepath = remotePath
	downloadReq.statusCallback = status
	downloadReq.downloadMask = ((1 << uint32(len(a.Blobbers))) - 1)
	downloadReq.blobbers = a.Blobbers
	downloadReq.datashards = a.DataShards
	downloadReq.parityshards = a.ParityShards
	downloadReq.numBlocks = int64(numBlockDownloads)
	downloadReq.consensusThresh = (float32(a.DataShards) * 100) / float32(a.DataShards+a.ParityShards)
	downloadReq.fullconsensus = float32(a.DataShards + a.ParityShards)
	downloadReq.completedCallback = func(remotepath string, remotepathhash string) {
		a.mutex.Lock()
		defer a.mutex.Unlock()
		delete(a.downloadProgressMap, remotepath)
	}
	downloadReq.contentMode = contentMode
	go func() {
		a.downloadChan <- downloadReq
		a.mutex.Lock()
		defer a.mutex.Unlock()
		a.downloadProgressMap[remotePath] = downloadReq
	}()
	return nil
}

func (a *Allocation) ListDirFromAuthTicket(authTicket string, lookupHash string) (*ListResult, error) {
	if !a.isInitialized() {
		return nil, notInitialized
	}
	sEnc, err := base64.StdEncoding.DecodeString(authTicket)
	if err != nil {
		return nil, common.NewError("auth_ticket_decode_error", "Error decoding the auth ticket."+err.Error())
	}
	at := &marker.AuthTicket{}
	err = json.Unmarshal(sEnc, at)
	if err != nil {
		return nil, common.NewError("auth_ticket_decode_error", "Error unmarshaling the auth ticket."+err.Error())
	}
	if len(at.FilePathHash) == 0 || len(lookupHash) == 0 {
		return nil, common.NewError("invalid_path", "Invalid path for the list")
	}

	listReq := &ListRequest{}
	listReq.allocationID = a.ID
	listReq.allocationTx = a.Tx
	listReq.blobbers = a.Blobbers
	listReq.consensusThresh = (float32(a.DataShards) * 100) / float32(a.DataShards+a.ParityShards)
	listReq.fullconsensus = float32(a.DataShards + a.ParityShards)
	listReq.ctx = a.ctx
	listReq.remotefilepathhash = lookupHash
	listReq.authToken = at
	ref := listReq.GetListFromBlobbers()
	if ref != nil {
		return ref, nil
	}
	return nil, common.NewError("list_request_failed", "Failed to get list response from the blobbers")
}

func (a *Allocation) ListDir(path string) (*ListResult, error) {
	if !a.isInitialized() {
		return nil, notInitialized
	}
	if len(path) == 0 {
		return nil, common.NewError("invalid_path", "Invalid path for the list")
	}
	path = zboxutil.RemoteClean(path)
	isabs := zboxutil.IsRemoteAbs(path)
	if !isabs {
		return nil, common.NewError("invalid_path", "Path should be valid and absolute")
	}
	listReq := &ListRequest{}
	listReq.allocationID = a.ID
	listReq.allocationTx = a.Tx
	listReq.blobbers = a.Blobbers
	listReq.consensusThresh = (float32(a.DataShards) * 100) / float32(a.DataShards+a.ParityShards)
	listReq.fullconsensus = float32(a.DataShards + a.ParityShards)
	listReq.ctx = a.ctx
	listReq.remotefilepath = path
	ref := listReq.GetListFromBlobbers()
	if ref != nil {
		return ref, nil
	}
	return nil, common.NewError("list_request_failed", "Failed to get list response from the blobbers")
}

func (a *Allocation) GetFileMeta(path string) (*ConsolidatedFileMeta, error) {
	result := &ConsolidatedFileMeta{}
	listReq := &ListRequest{}
	listReq.allocationID = a.ID
	listReq.allocationTx = a.Tx
	listReq.blobbers = a.Blobbers
	listReq.consensusThresh = (float32(a.DataShards) * 100) / float32(a.DataShards+a.ParityShards)
	listReq.fullconsensus = float32(a.DataShards + a.ParityShards)
	listReq.ctx = a.ctx
	listReq.remotefilepath = path
	_, ref, _ := listReq.getFileConsensusFromBlobbers()
	if ref != nil {
		result.Type = ref.Type
		result.Name = ref.Name
		result.Hash = ref.ActualFileHash
		result.LookupHash = ref.LookupHash
		result.MimeType = ref.MimeType
		result.Path = ref.Path
		result.Size = ref.ActualFileSize
		result.EncryptedKey = ref.EncryptedKey
		result.CommitMetaTxns = ref.CommitMetaTxns
		return result, nil
	}
	return nil, common.NewError("file_meta_error", "Error getting the file meta data from blobbers")
}

func (a *Allocation) GetFileMetaFromAuthTicket(authTicket string, lookupHash string) (*ConsolidatedFileMeta, error) {
	result := &ConsolidatedFileMeta{}
	sEnc, err := base64.StdEncoding.DecodeString(authTicket)
	if err != nil {
		return nil, common.NewError("auth_ticket_decode_error", "Error decoding the auth ticket."+err.Error())
	}
	at := &marker.AuthTicket{}
	err = json.Unmarshal(sEnc, at)
	if err != nil {
		return nil, common.NewError("auth_ticket_decode_error", "Error unmarshaling the auth ticket."+err.Error())
	}
	if len(at.FilePathHash) == 0 || len(lookupHash) == 0 {
		return nil, common.NewError("invalid_path", "Invalid path for the list")
	}

	listReq := &ListRequest{}
	listReq.allocationID = a.ID
	listReq.allocationTx = a.Tx
	listReq.blobbers = a.Blobbers
	listReq.consensusThresh = (float32(a.DataShards) * 100) / float32(a.DataShards+a.ParityShards)
	listReq.fullconsensus = float32(a.DataShards + a.ParityShards)
	listReq.ctx = a.ctx
	listReq.remotefilepathhash = lookupHash
	listReq.authToken = at
	_, ref, _ := listReq.getFileConsensusFromBlobbers()
	if ref != nil {
		result.Type = ref.Type
		result.Name = ref.Name
		result.Hash = ref.ActualFileHash
		result.LookupHash = ref.LookupHash
		result.MimeType = ref.MimeType
		result.Path = ref.Path
		result.Size = ref.ActualFileSize
		result.CommitMetaTxns = ref.CommitMetaTxns
		return result, nil
	}
	return nil, common.NewError("file_meta_error", "Error getting the file meta data from blobbers")
}

func (a *Allocation) GetFileStats(path string) (map[string]*FileStats, error) {
	if !a.isInitialized() {
		return nil, notInitialized
	}
	if len(path) == 0 {
		return nil, common.NewError("invalid_path", "Invalid path for the list")
	}
	path = zboxutil.RemoteClean(path)
	isabs := zboxutil.IsRemoteAbs(path)
	if !isabs {
		return nil, common.NewError("invalid_path", "Path should be valid and absolute")
	}
	listReq := &ListRequest{}
	listReq.allocationID = a.ID
	listReq.allocationTx = a.Tx
	listReq.blobbers = a.Blobbers
	listReq.consensusThresh = (float32(a.DataShards) * 100) / float32(a.DataShards+a.ParityShards)
	listReq.fullconsensus = float32(a.DataShards + a.ParityShards)
	listReq.ctx = a.ctx
	listReq.remotefilepath = path
	ref := listReq.getFileStatsFromBlobbers()
	if ref != nil {
		return ref, nil
	}
	return nil, common.NewError("file_stats_request_failed", "Failed to get file stats response from the blobbers")
}

func (a *Allocation) DeleteFile(path string) error {
	if !a.isInitialized() {
		return notInitialized
	}
	if a.UnderRepair() {
		return underRepair
	}
	if len(path) == 0 {
		return common.NewError("invalid_path", "Invalid path for the list")
	}
	path = zboxutil.RemoteClean(path)
	isabs := zboxutil.IsRemoteAbs(path)
	if !isabs {
		return common.NewError("invalid_path", "Path should be valid and absolute")
	}

	req := &DeleteRequest{}
	req.blobbers = a.Blobbers
	req.allocationID = a.ID
	req.allocationTx = a.Tx
	req.consensusThresh = (float32(a.DataShards) * 100) / float32(a.DataShards+a.ParityShards)
	req.fullconsensus = float32(a.DataShards + a.ParityShards)
	req.ctx = a.ctx
	req.remotefilepath = path
	req.deleteMask = 0
	req.listMask = 0
	req.connectionID = zboxutil.NewConnectionId()
	err := req.ProcessDelete()
	return err
}

func (a *Allocation) RenameObject(path string, destName string) error {
	if !a.isInitialized() {
		return notInitialized
	}

	if a.UnderRepair() {
		return underRepair
	}

	if len(path) == 0 {
		return common.NewError("invalid_path", "Invalid path for the list")
	}
	path = zboxutil.RemoteClean(path)
	isabs := zboxutil.IsRemoteAbs(path)
	if !isabs {
		return common.NewError("invalid_path", "Path should be valid and absolute")
	}

	req := &RenameRequest{}
	req.blobbers = a.Blobbers
	req.allocationID = a.ID
	req.allocationTx = a.Tx
	req.newName = destName
	req.consensusThresh = (float32(a.DataShards) * 100) / float32(a.DataShards+a.ParityShards)
	req.fullconsensus = float32(a.DataShards + a.ParityShards)
	req.ctx = a.ctx
	req.remotefilepath = path
	req.renameMask = 0
	req.connectionID = zboxutil.NewConnectionId()
	err := req.ProcessRename()
	return err
}

func (a *Allocation) MoveObject(path string, destPath string) error {
	err := a.CopyObject(path, destPath)
	if err != nil {
		return err
	}
	return a.DeleteFile(path)
}

func (a *Allocation) CopyObject(path string, destPath string) error {
	if !a.isInitialized() {
		return notInitialized
	}

	if a.UnderRepair() {
		return underRepair
	}

	if len(path) == 0 || len(destPath) == 0 {
		return common.NewError("invalid_path", "Invalid path for copy")
	}
	path = zboxutil.RemoteClean(path)
	isabs := zboxutil.IsRemoteAbs(path)
	if !isabs {
		return common.NewError("invalid_path", "Path should be valid and absolute")
	}

	req := &CopyRequest{}
	req.blobbers = a.Blobbers
	req.allocationID = a.ID
	req.allocationTx = a.Tx
	req.destPath = destPath
	req.consensusThresh = (float32(a.DataShards) * 100) / float32(a.DataShards+a.ParityShards)
	req.fullconsensus = float32(a.DataShards + a.ParityShards)
	req.ctx = a.ctx
	req.remotefilepath = path
	req.copyMask = 0
	req.connectionID = zboxutil.NewConnectionId()
	err := req.ProcessCopy()
	return err
}

func (a *Allocation) GetAuthTicketForShare(path string, filename string, referenceType string, refereeClientID string) (string, error) {
	return a.GetAuthTicket(path, filename, referenceType, refereeClientID, "")
}

func (a *Allocation) GetAuthTicket(path string, filename string, referenceType string, refereeClientID string, refereeEncryptionPublicKey string) (string, error) {
	if !a.isInitialized() {
		return "", notInitialized
	}
	if len(path) == 0 {
		return "", common.NewError("invalid_path", "Invalid path for the list")
	}
	path = zboxutil.RemoteClean(path)
	isabs := zboxutil.IsRemoteAbs(path)
	if !isabs {
		return "", common.NewError("invalid_path", "Path should be valid and absolute")
	}

	shareReq := &ShareRequest{}
	shareReq.allocationID = a.ID
	shareReq.blobbers = a.Blobbers
	shareReq.ctx = a.ctx
	shareReq.remotefilepath = path
	shareReq.remotefilename = filename
	if referenceType == fileref.DIRECTORY {
		shareReq.refType = fileref.DIRECTORY
	} else {
		shareReq.refType = fileref.FILE
	}
	if len(refereeEncryptionPublicKey) > 0 {
		authTicket, err := shareReq.GetAuthTicketForEncryptedFile(refereeClientID, refereeEncryptionPublicKey)
		if err != nil {
			return "", err
		}
		return authTicket, nil

	}
	authTicket, err := shareReq.GetAuthTicket(refereeClientID)
	if err != nil {
		return "", err
	}
	return authTicket, nil
}

func (a *Allocation) CancelUpload(localpath string) error {
	if uploadReq, ok := a.uploadProgressMap[localpath]; ok {
		uploadReq.isUploadCanceled = true
		return nil
	}
	return common.NewError("local_path_not_found", "Invalid path. Do upload in progress for the path "+localpath)
}

func (a *Allocation) CancelDownload(remotepath string) error {
	if downloadReq, ok := a.downloadProgressMap[remotepath]; ok {
		downloadReq.isDownloadCanceled = true
		return nil
	}
	return common.NewError("remote_path_not_found", "Invalid path. Do download in progress for the path "+remotepath)
}

func (a *Allocation) DownloadThumbnailFromAuthTicket(localPath string, authTicket string, remoteLookupHash string, remoteFilename string, status StatusCallback) error {
	return a.downloadFromAuthTicket(localPath, authTicket, remoteLookupHash, remoteFilename, DOWNLOAD_CONTENT_THUMB, status)
}

func (a *Allocation) DownloadFromAuthTicket(localPath string, authTicket string, remoteLookupHash string, remoteFilename string, status StatusCallback) error {
	return a.downloadFromAuthTicket(localPath, authTicket, remoteLookupHash, remoteFilename, DOWNLOAD_CONTENT_FULL, status)
}

func (a *Allocation) downloadFromAuthTicket(localPath string, authTicket string, remoteLookupHash string, remoteFilename string, contentMode string, status StatusCallback) error {
	if !a.isInitialized() {
		return notInitialized
	}
	if a.UnderRepair() {
		return underRepair
	}
	sEnc, err := base64.StdEncoding.DecodeString(authTicket)
	if err != nil {
		return common.NewError("auth_ticket_decode_error", "Error decoding the auth ticket."+err.Error())
	}
	at := &marker.AuthTicket{}
	err = json.Unmarshal(sEnc, at)
	if err != nil {
		return common.NewError("auth_ticket_decode_error", "Error unmarshaling the auth ticket."+err.Error())
	}
	if stat, err := os.Stat(localPath); err == nil {
		if !stat.IsDir() {
			return fmt.Errorf("Local path is not a directory '%s'", localPath)
		}
		localPath = strings.TrimRight(localPath, "/")
		_, rFile := filepath.Split(remoteFilename)
		localPath = fmt.Sprintf("%s/%s", localPath, rFile)
		if _, err := os.Stat(localPath); err == nil {
			return fmt.Errorf("Local file already exists '%s'", localPath)
		}
	}
	if len(a.Blobbers) <= 1 {
		return noBLOBBERS
	}

	downloadReq := &DownloadRequest{}
	downloadReq.allocationID = a.ID
	downloadReq.allocationTx = a.Tx
	downloadReq.ctx, _ = context.WithCancel(a.ctx)
	downloadReq.localpath = localPath
	downloadReq.remotefilepathhash = remoteLookupHash
	downloadReq.authTicket = at
	downloadReq.statusCallback = status
	downloadReq.downloadMask = ((1 << uint32(len(a.Blobbers))) - 1)
	downloadReq.blobbers = a.Blobbers
	downloadReq.datashards = a.DataShards
	downloadReq.parityshards = a.ParityShards
	downloadReq.contentMode = contentMode
	downloadReq.numBlocks = int64(numBlockDownloads)
	downloadReq.consensusThresh = (float32(a.DataShards) * 100) / float32(a.DataShards+a.ParityShards)
	downloadReq.fullconsensus = float32(a.DataShards + a.ParityShards)
	downloadReq.completedCallback = func(remotepath string, remotepathHash string) {
		a.mutex.Lock()
		defer a.mutex.Unlock()
		delete(a.downloadProgressMap, remotepathHash)
	}
	go func() {
		a.downloadChan <- downloadReq
		a.mutex.Lock()
		defer a.mutex.Unlock()
		a.downloadProgressMap[remoteLookupHash] = downloadReq
	}()
	return nil
}

func (a *Allocation) CommitMetaTransaction(path, crudOperation, authTicket, lookupHash string, fileMeta *ConsolidatedFileMeta, status StatusCallback) (err error) {
	if !a.isInitialized() {
		return notInitialized
	}

	if a.UnderRepair() {
		return underRepair
	}

	if fileMeta == nil {
		if len(path) > 0 {
			fileMeta, err = a.GetFileMeta(path)
			if err != nil {
				return err
			}
		} else if len(authTicket) > 0 {
			fileMeta, err = a.GetFileMetaFromAuthTicket(authTicket, lookupHash)
			if err != nil {
				return err
			}
		}
	}

	req := &CommitMetaRequest{
		CommitMetaData: CommitMetaData{
			CrudType: crudOperation,
			MetaData: fileMeta,
		},
		status:    status,
		a:         a,
		authToken: authTicket,
	}
	go req.processCommitMetaRequest()
	return nil
}
