package recording

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/db"
	"github.com/google/uuid"
)

const (
	SampleRate      = 16000
	Channels        = 1
	Bitrate         = 32000
	WaveformSamples = 320
	DefaultMinFree  = int64(500 * 1024 * 1024)
)

var (
	ErrNotActive                  = errors.New("recording is not active")
	ErrLowStorage                 = errors.New("recording storage is below the minimum free-space threshold")
	ErrRecordingDisabled          = errors.New("recording is disabled")
	ErrAutomaticRecordingDisabled = errors.New("automatic recording requires certified recording-tone support")
)

type CallMeta struct {
	ID        string
	Direction string
	Peer      string
	StartedAt time.Time
}

type Encoder interface {
	Encode(context.Context, string, string, string) error
}

type Manager struct {
	database      *db.DB
	root          string
	retentionDays int
	minimumFree   int64
	encoder       Encoder
	now           func() time.Time

	mu       sync.Mutex
	sessions map[string]*session
	onEvent  func(string, db.Recording)
}

type session struct {
	mu              sync.Mutex
	recording       db.Recording
	workDir         string
	cellular        *os.File
	client          *os.File
	cellularSamples int64
	clientSamples   int64
}

func NewManager(database *db.DB, root string, retentionDays int) (*Manager, error) {
	return NewManagerWithEncoder(database, root, retentionDays, FFmpegEncoder{})
}

func NewManagerWithEncoder(database *db.DB, root string, retentionDays int, encoder Encoder) (*Manager, error) {
	if database == nil {
		return nil, fmt.Errorf("recording database is required")
	}
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("recording root is required")
	}
	if retentionDays < 0 {
		return nil, fmt.Errorf("recording retention days cannot be negative")
	}
	if encoder == nil {
		return nil, fmt.Errorf("recording encoder is required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve recording root: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "work"), 0o750); err != nil {
		return nil, fmt.Errorf("create recording root: %w", err)
	}
	manager := &Manager{
		database: database, root: root, retentionDays: retentionDays,
		minimumFree: DefaultMinFree, encoder: encoder, now: time.Now,
		sessions: make(map[string]*session),
	}
	if settings, settingsErr := database.GetRecordingSettings(context.Background()); settingsErr == nil {
		manager.retentionDays = settings.RetentionDays
		manager.minimumFree = settings.MinimumFreeBytes
	} else if errors.Is(settingsErr, sql.ErrNoRows) {
		if err := database.EnsureRecordingSettings(context.Background(), db.RecordingSettings{
			Enabled: true, AutoMode: "off", RetentionDays: retentionDays, MinimumFreeBytes: DefaultMinFree,
		}); err != nil {
			return nil, fmt.Errorf("initialize recording settings: %w", err)
		}
	} else {
		return nil, fmt.Errorf("read recording settings: %w", settingsErr)
	}
	return manager, nil
}

func (m *Manager) SetEventHandler(handler func(string, db.Recording)) {
	m.mu.Lock()
	m.onEvent = handler
	m.mu.Unlock()
}

func (m *Manager) SetMinimumFreeBytes(value int64) {
	if value < 0 {
		value = 0
	}
	m.mu.Lock()
	m.minimumFree = value
	m.mu.Unlock()
}

func (m *Manager) Settings(ctx context.Context) (db.RecordingSettings, error) {
	settings, err := m.database.GetRecordingSettings(ctx)
	if err == nil {
		return settings, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return db.RecordingSettings{}, err
	}
	m.mu.Lock()
	settings = db.RecordingSettings{Enabled: true, AutoMode: "off", RetentionDays: m.retentionDays, MinimumFreeBytes: m.minimumFree}
	m.mu.Unlock()
	return settings, nil
}

func (m *Manager) UpdateSettings(ctx context.Context, settings db.RecordingSettings) error {
	if settings.RetentionDays < 0 || settings.MinimumFreeBytes < 0 {
		return fmt.Errorf("recording retention and minimum free space cannot be negative")
	}
	if settings.AutoMode == "" {
		settings.AutoMode = "off"
	}
	if settings.AutoMode != "off" {
		return ErrAutomaticRecordingDisabled
	}
	settings.UpdatedAt = m.now()
	if err := m.database.SaveRecordingSettings(ctx, settings); err != nil {
		return err
	}
	m.mu.Lock()
	m.retentionDays = settings.RetentionDays
	m.minimumFree = settings.MinimumFreeBytes
	m.mu.Unlock()
	return nil
}

func (m *Manager) Start(ctx context.Context, call CallMeta, mode string, deviceID string) (db.Recording, error) {
	if err := ctx.Err(); err != nil {
		return db.Recording{}, err
	}
	if call.ID == "" {
		return db.Recording{}, fmt.Errorf("call id is required")
	}
	if mode == "" {
		mode = "manual"
	}
	m.mu.Lock()
	if current := m.sessions[call.ID]; current != nil {
		value := current.recording
		m.mu.Unlock()
		return value, nil
	}
	minimumFree := m.minimumFree
	m.mu.Unlock()
	settings, settingsErr := m.Settings(ctx)
	if settingsErr != nil {
		return db.Recording{}, settingsErr
	}
	if !settings.Enabled {
		return db.Recording{}, ErrRecordingDisabled
	}
	if mode != "manual" && settings.AutoMode == "off" {
		return db.Recording{}, ErrAutomaticRecordingDisabled
	}
	if free, err := availableSpace(m.root); err != nil {
		return db.Recording{}, fmt.Errorf("check recording storage: %w", err)
	} else if free < minimumFree {
		return db.Recording{}, ErrLowStorage
	}
	if existing, err := m.database.GetRecordingByCall(ctx, call.ID); err == nil {
		if existing.State == "recording" || existing.State == "starting" {
			return existing, nil
		}
		if existing.State == "finalizing" {
			return db.Recording{}, fmt.Errorf("call recording is still finalizing")
		}
		// Ready and failed segments are retained in the recording list, but do
		// not prevent the user from starting another segment in this call.
	} else if !errors.Is(err, sql.ErrNoRows) {
		return db.Recording{}, err
	}

	id := uuid.NewString()
	created := m.now()
	started := call.StartedAt
	if started.IsZero() {
		started = created
	}
	finalPath := filepath.Join(m.root, started.Format("2006"), started.Format("01"), id+".m4a")
	workDir := filepath.Join(m.root, "work", id)
	if err := os.MkdirAll(workDir, 0o750); err != nil {
		return db.Recording{}, fmt.Errorf("create recording work directory: %w", err)
	}
	cellular, err := os.OpenFile(filepath.Join(workDir, "cellular.pcm.part"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return db.Recording{}, fmt.Errorf("open cellular recording track: %w", err)
	}
	client, err := os.OpenFile(filepath.Join(workDir, "client.pcm.part"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		_ = cellular.Close()
		return db.Recording{}, fmt.Errorf("open client recording track: %w", err)
	}
	recording := db.Recording{
		ID: id, CallID: call.ID, State: "starting", TriggerMode: mode,
		StartedAt: &started, FilePath: finalPath, Container: "m4a", Codec: "aac-lc",
		SampleRate: SampleRate, Channels: Channels, Bitrate: Bitrate,
		RetentionExpiresAt: retentionDeadline(created, m.retentionDays), CreatedByDeviceID: deviceID,
		CreatedAt: created, UpdatedAt: created,
	}
	sequence, err := m.database.SaveRecording(ctx, recording)
	if err != nil {
		_ = cellular.Close()
		_ = client.Close()
		return db.Recording{}, err
	}
	recording.SyncSeq = sequence
	if err := m.database.UpdateCallRecording(ctx, call.ID, id, recording.State, nil); err != nil {
		_ = cellular.Close()
		_ = client.Close()
		return db.Recording{}, err
	}
	m.publish("recording.starting", recording)
	recording.State, recording.UpdatedAt = "recording", m.now()
	sequence, err = m.database.SaveRecording(ctx, recording)
	if err != nil {
		_ = cellular.Close()
		_ = client.Close()
		return db.Recording{}, err
	}
	recording.SyncSeq = sequence
	if err := m.database.UpdateCallRecording(ctx, call.ID, id, recording.State, nil); err != nil {
		_ = cellular.Close()
		_ = client.Close()
		return db.Recording{}, err
	}
	meta := map[string]any{"id": id, "callId": call.ID, "state": recording.State, "createdAt": created.UnixMilli()}
	if data, marshalErr := json.Marshal(meta); marshalErr == nil {
		_ = os.WriteFile(filepath.Join(workDir, "meta.json"), data, 0o640)
	}
	m.mu.Lock()
	m.sessions[call.ID] = &session{recording: recording, workDir: workDir, cellular: cellular, client: client}
	m.mu.Unlock()
	m.publish("recording.started", recording)
	return recording, nil
}

func (m *Manager) WriteCellularFrame(callID string, pcm []int16) error {
	return m.write(callID, pcm, true)
}

func (m *Manager) WriteClientFrame(callID string, pcm []int16) error {
	return m.write(callID, pcm, false)
}

func (m *Manager) write(callID string, pcm []int16, cellular bool) error {
	m.mu.Lock()
	current := m.sessions[callID]
	m.mu.Unlock()
	if current == nil {
		return ErrNotActive
	}
	if len(pcm) == 0 {
		return nil
	}
	upsampled := upsample8To16(pcm)
	buffer := make([]byte, len(upsampled)*2)
	for index, sample := range upsampled {
		binary.LittleEndian.PutUint16(buffer[index*2:index*2+2], uint16(sample))
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	file := current.client
	if cellular {
		file = current.cellular
	}
	if file == nil {
		return ErrNotActive
	}
	if _, err := file.Write(buffer); err != nil {
		return err
	}
	if cellular {
		current.cellularSamples += int64(len(upsampled))
	} else {
		current.clientSamples += int64(len(upsampled))
	}
	return nil
}

func (m *Manager) Stop(ctx context.Context, callID, reason string) error {
	m.mu.Lock()
	current := m.sessions[callID]
	delete(m.sessions, callID)
	m.mu.Unlock()
	if current == nil {
		return ErrNotActive
	}
	current.mu.Lock()
	if current.cellular != nil {
		_ = current.cellular.Sync()
		_ = current.cellular.Close()
	}
	if current.client != nil {
		_ = current.client.Sync()
		_ = current.client.Close()
	}
	current.cellular, current.client = nil, nil
	cellularSamples, clientSamples := current.cellularSamples, current.clientSamples
	recording := current.recording
	current.mu.Unlock()
	now := m.now()
	recording.State, recording.StoppedAt, recording.UpdatedAt = "finalizing", &now, now
	recording.DurationMs = durationMillis(cellularSamples, clientSamples)
	sequence, err := m.database.SaveRecording(ctx, recording)
	if err != nil {
		return err
	}
	recording.SyncSeq = sequence
	_ = m.database.UpdateCallRecording(ctx, callID, recording.ID, recording.State, recording.DurationMs)
	m.publish("recording.finalizing", recording)
	go m.finalize(recording, current.workDir, reason)
	return nil
}

func (m *Manager) finalize(recording db.Recording, workDir, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cellularPath := filepath.Join(workDir, "cellular.pcm.part")
	clientPath := filepath.Join(workDir, "client.pcm.part")
	if err := os.MkdirAll(filepath.Dir(recording.FilePath), 0o750); err != nil {
		m.fail(recording, "storage_error", err)
		return
	}
	// Keep the format extension at the end of the temporary filename. FFmpeg
	// selects its muxer from the output extension; `.m4a.part` is therefore
	// treated as an unknown format and every real recording fails at finalize.
	temporary := recording.FilePath + ".part.m4a"
	_ = os.Remove(temporary)
	if err := m.encoder.Encode(ctx, cellularPath, clientPath, temporary); err != nil {
		_ = os.Remove(temporary)
		m.fail(recording, "encoder_failed", err)
		return
	}
	waveformPath := recording.FilePath + ".waveform.json"
	if err := writeWaveform(waveformPath, cellularPath, clientPath); err != nil {
		_ = os.Remove(temporary)
		m.fail(recording, "waveform_failed", err)
		return
	}
	if err := os.Rename(temporary, recording.FilePath); err != nil {
		_ = os.Remove(waveformPath)
		m.fail(recording, "storage_error", err)
		return
	}
	size, err := fileSize(recording.FilePath)
	if err != nil {
		m.fail(recording, "storage_error", err)
		return
	}
	hash, err := fileSHA256(recording.FilePath)
	if err != nil {
		m.fail(recording, "storage_error", err)
		return
	}
	now := m.now()
	recording.State, recording.WaveformPath, recording.SizeBytes, recording.SHA256, recording.UpdatedAt = "ready", waveformPath, &size, hash, now
	sequence, err := m.database.SaveRecording(context.Background(), recording)
	if err != nil {
		m.fail(recording, "database_error", err)
		return
	}
	recording.SyncSeq = sequence
	_ = m.database.UpdateCallRecording(context.Background(), recording.CallID, recording.ID, recording.State, recording.DurationMs)
	_ = os.Remove(cellularPath)
	_ = os.Remove(clientPath)
	_ = os.Remove(filepath.Join(workDir, "meta.json"))
	_ = os.Remove(workDir)
	_ = reason // retained for future audit detail without changing the stable API
	m.publish("recording.ready", recording)
}

func (m *Manager) fail(recording db.Recording, code string, err error) {
	now := m.now()
	recording.State, recording.FailureCode, recording.FailureDetail, recording.UpdatedAt = "failed", code, err.Error(), now
	slog.Error("recording failed", "recording_id", recording.ID, "call_id", recording.CallID, "failure_code", code, "error", err)
	if sequence, saveErr := m.database.SaveRecording(context.Background(), recording); saveErr == nil {
		recording.SyncSeq = sequence
	}
	_ = m.database.UpdateCallRecording(context.Background(), recording.CallID, recording.ID, recording.State, recording.DurationMs)
	m.publish("recording.failed", recording)
}

func (m *Manager) Get(ctx context.Context, id string) (db.Recording, error) {
	return m.database.GetRecording(ctx, id)
}

func (m *Manager) List(ctx context.Context, after int64, limit int, direction, query string, from, to *time.Time) ([]db.Recording, error) {
	return m.database.ListRecordings(ctx, after, limit, direction, query, from, to)
}

func (m *Manager) UsedBytes() (int64, error) {
	var total int64
	err := filepath.Walk(m.root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func (m *Manager) Delete(ctx context.Context, id string) error {
	recording, err := m.database.GetRecording(ctx, id)
	if err != nil {
		return err
	}
	if recording.State == "deleted" {
		return nil
	}
	now := m.now()
	recording.State, recording.UpdatedAt = "deleted", now
	sequence, err := m.database.SaveRecording(ctx, recording)
	if err != nil {
		return err
	}
	recording.SyncSeq = sequence
	_ = m.database.UpdateCallRecording(ctx, recording.CallID, recording.ID, recording.State, recording.DurationMs)
	if recording.FilePath != "" {
		_ = os.Remove(recording.FilePath)
	}
	if recording.WaveformPath != "" {
		_ = os.Remove(recording.WaveformPath)
	}
	_ = os.Remove(filepath.Join(m.root, "work", id))
	m.publish("recording.deleted", recording)
	return nil
}

func (m *Manager) AudioPath(ctx context.Context, id string) (string, error) {
	recording, err := m.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if recording.State != "ready" || recording.FilePath == "" {
		return "", os.ErrNotExist
	}
	return m.safePath(recording.FilePath)
}

func (m *Manager) WaveformPath(ctx context.Context, id string) (string, error) {
	recording, err := m.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if recording.State != "ready" || recording.WaveformPath == "" {
		return "", os.ErrNotExist
	}
	return m.safePath(recording.WaveformPath)
}

func (m *Manager) RunRetention(ctx context.Context) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mu.Lock()
			retentionDays := m.retentionDays
			m.mu.Unlock()
			if retentionDays > 0 {
				m.deleteExpired(ctx)
			}
		}
	}
}

// Recover reconciles work directories left by a Gateway restart. A completed
// call is finalized from its durable PCM parts; an in-progress call is marked
// failed because the old media session cannot safely be resumed. Unknown work
// directories are retained for one day for diagnostics before cleanup.
func (m *Manager) Recover(ctx context.Context) error {
	entries, err := os.ReadDir(filepath.Join(m.root, "work"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		recording, getErr := m.database.GetRecording(ctx, id)
		if errors.Is(getErr, sql.ErrNoRows) {
			info, statErr := entry.Info()
			if statErr == nil && m.now().Sub(info.ModTime()) > 24*time.Hour {
				_ = os.RemoveAll(filepath.Join(m.root, "work", id))
			}
			continue
		}
		if getErr != nil {
			return getErr
		}
		call, callErr := m.database.GetCall(ctx, recording.CallID)
		if callErr != nil {
			m.fail(recording, "orphaned_call", callErr)
			continue
		}
		if call.EndedAt == nil {
			m.fail(recording, "gateway_restarted", errors.New("recording media session was interrupted by Gateway restart"))
			continue
		}
		cellularPath := filepath.Join(m.root, "work", id, "cellular.pcm.part")
		clientPath := filepath.Join(m.root, "work", id, "client.pcm.part")
		cellularSize, _ := fileSize(cellularPath)
		clientSize, _ := fileSize(clientPath)
		recording.DurationMs = durationMillis(cellularSize/2, clientSize/2)
		if recording.StoppedAt == nil {
			recording.StoppedAt = call.EndedAt
		}
		go m.finalize(recording, filepath.Join(m.root, "work", id), "recovery")
	}
	return nil
}

func (m *Manager) deleteExpired(ctx context.Context) {
	// The daily job uses the bounded list API and deletes only records whose
	// server-side expiry has passed; no client-provided path is ever trusted.
	recordings, err := m.database.ListRecordings(ctx, 0, 500, "", "", nil, nil)
	if err != nil {
		return
	}
	now := m.now()
	for _, recording := range recordings {
		if recording.RetentionExpiresAt != nil && now.After(*recording.RetentionExpiresAt) {
			_ = m.Delete(ctx, recording.ID)
		}
	}
}

func (m *Manager) safePath(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(m.root, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("recording path is outside recording root")
	}
	return absolute, nil
}

func (m *Manager) publish(kind string, recording db.Recording) {
	m.mu.Lock()
	handler := m.onEvent
	m.mu.Unlock()
	if handler != nil {
		handler(kind, recording)
	}
}

// FFmpegEncoder emits the user-facing V1 M4A/AAC-LC file. Raw PCM stays in
// the work directory until the atomic final rename has succeeded.
type FFmpegEncoder struct{}

func (FFmpegEncoder) Encode(ctx context.Context, cellular, client, output string) error {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("ffmpeg is unavailable: %w", err)
	}
	command := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "s16le", "-ar", "16000", "-ac", "1", "-i", cellular,
		"-f", "s16le", "-ar", "16000", "-ac", "1", "-i", client,
		"-filter_complex", "[0:a][1:a]amix=inputs=2:duration=longest:normalize=0,volume=0.5,alimiter=limit=0.95",
		"-c:a", "aac", "-profile:a", "aac_low", "-b:a", "32k", "-movflags", "+faststart", "-y", output)
	outputBytes, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("encode M4A: %w: %s", err, strings.TrimSpace(string(outputBytes)))
	}
	return nil
}

func upsample8To16(samples []int16) []int16 {
	result := make([]int16, len(samples)*2)
	for index, sample := range samples {
		result[index*2], result[index*2+1] = sample, sample
	}
	return result
}

func availableSpace(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}

func retentionDeadline(created time.Time, days int) *time.Time {
	if days <= 0 {
		return nil
	}
	value := created.Add(time.Duration(days) * 24 * time.Hour)
	return &value
}

func durationMillis(cellular, client int64) *int64 {
	samples := cellular
	if client > samples {
		samples = client
	}
	if samples <= 0 {
		return nil
	}
	value := samples * 1000 / SampleRate
	return &value
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type waveform struct {
	SampleCount int       `json:"sampleCount"`
	Peaks       []float32 `json:"peaks"`
}

func writeWaveform(path, cellular, client string) error {
	cellularPeaks, err := trackPeaks(cellular)
	if err != nil {
		return err
	}
	clientPeaks, err := trackPeaks(client)
	if err != nil {
		return err
	}
	peaks := make([]float32, WaveformSamples)
	for index := range peaks {
		if cellularPeaks[index] > clientPeaks[index] {
			peaks[index] = cellularPeaks[index]
		} else {
			peaks[index] = clientPeaks[index]
		}
	}
	data, err := json.Marshal(waveform{SampleCount: WaveformSamples, Peaks: peaks})
	if err != nil {
		return err
	}
	temporary := path + ".part"
	if err := os.WriteFile(temporary, data, 0o640); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func trackPeaks(path string) ([]float32, error) {
	result := make([]float32, WaveformSamples)
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	total := info.Size() / 2
	if total == 0 {
		return result, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64*1024)
	buffer := make([]byte, 64*1024)
	var index int64
	for index < total {
		want := int((total - index) * 2)
		if want > len(buffer) {
			want = len(buffer)
		}
		if _, err := io.ReadFull(reader, buffer[:want]); err != nil {
			return nil, err
		}
		for offset := 0; offset < want; offset += 2 {
			value := math.Abs(float64(int16(binary.LittleEndian.Uint16(buffer[offset:offset+2]))) / 32768)
			bucket := int(index * WaveformSamples / total)
			if bucket >= WaveformSamples {
				bucket = WaveformSamples - 1
			}
			if float32(value) > result[bucket] {
				result[bucket] = float32(value)
			}
			index++
		}
	}
	return result, nil
}
