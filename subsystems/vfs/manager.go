package vfs

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/cyber-shuttle/linkspan/subsystems/vfs/export"
	"github.com/cyber-shuttle/linkspan/subsystems/vfs/frpclient"
	pb "github.com/cyber-shuttle/linkspan/subsystems/vfs/proto/gen/remotefs"
	"github.com/cyber-shuttle/linkspan/subsystems/vfs/source"
)

// MountConfig holds options for a FUSE mount.
type MountConfig struct {
	ServerAddr    string `json:"server_addr"`
	Mountpoint    string `json:"mountpoint"`
	// Token is "id:secret" from publish when using FRP; use with FRPConnection.
	Token         string `json:"token,omitempty"`
	FRPConnection string `json:"frp_connection,omitempty"`
	CacheSizeMB   int64  `json:"cache_size_mb,omitempty"`
	CacheTTLSec   int    `json:"cache_ttl_sec,omitempty"`
	BlockSizeKB   int64  `json:"block_size_kb,omitempty"`
	NoCache       bool   `json:"no_cache,omitempty"`
	CacheDir      string `json:"cache_dir,omitempty"`
	FileCacheMB   int64  `json:"file_cache_mb,omitempty"`
	Passthrough   bool   `json:"passthrough,omitempty"`
	MmapCache     bool   `json:"mmap_cache,omitempty"`
	AllowOther    bool   `json:"allow_other,omitempty"`
}

// MountEntry represents an active FUSE mount.
type MountEntry struct {
	ID         string    `json:"id"`
	Mountpoint string    `json:"mountpoint"`
	ServerAddr string    `json:"server_addr"`
	CreatedAt  time.Time `json:"created_at"`
	unmount    func() error
}

// MountManager manages active FUSE mounts.
type MountManager struct {
	mu     sync.Mutex
	nextID int
	mounts map[string]*MountEntry
}

// GlobalMountManager is the default mount manager.
var GlobalMountManager = &MountManager{mounts: make(map[string]*MountEntry)}

// Mount starts a FUSE mount. Returns mount ID or error. On non-Linux, returns ErrUnsupported.
func (m *MountManager) Mount(cfg MountConfig) (string, error) {
	unmount, err := doMount(cfg)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	mountID := fmt.Sprintf("mount-%d", m.nextID)
	m.mounts[mountID] = &MountEntry{
		ID:         mountID,
		Mountpoint: cfg.Mountpoint,
		ServerAddr: cfg.ServerAddr,
		CreatedAt:  time.Now(),
		unmount:    unmount,
	}
	return mountID, nil
}

// Unmount unmounts the given mount by ID.
func (m *MountManager) Unmount(id string) error {
	m.mu.Lock()
	ent, ok := m.mounts[id]
	if ok {
		delete(m.mounts, id)
	}
	m.mu.Unlock()
	if !ok {
		return ErrMountNotFound
	}
	if ent.unmount != nil {
		return ent.unmount()
	}
	return nil
}

// List returns all active mounts.
func (m *MountManager) List() []MountEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MountEntry, 0, len(m.mounts))
	for _, e := range m.mounts {
		out = append(out, MountEntry{ID: e.ID, Mountpoint: e.Mountpoint, ServerAddr: e.ServerAddr, CreatedAt: e.CreatedAt})
	}
	return out
}

// UnmountAll unmounts all active mounts.
func (m *MountManager) UnmountAll() {
	m.mu.Lock()
	list := make([]*MountEntry, 0, len(m.mounts))
	for _, e := range m.mounts {
		list = append(list, e)
	}
	m.mounts = make(map[string]*MountEntry)
	m.mu.Unlock()
	for _, e := range list {
		if e.unmount != nil {
			_ = e.unmount()
		}
	}
}

// PublishEntry represents an active gRPC publish server.
type PublishEntry struct {
	ID        string    `json:"id"`
	Folder    string    `json:"folder"`
	ListenAddr string   `json:"listen_addr"`
	CreatedAt time.Time `json:"created_at"`
	stop      func()
}

// PublishManager manages active gRPC publish servers.
type PublishManager struct {
	mu        sync.Mutex
	nextID    int
	publishes map[string]*PublishEntry
}

// GlobalPublishManager is the default publish manager.
var GlobalPublishManager = &PublishManager{publishes: make(map[string]*PublishEntry)}

// PublishConfig holds options for publishing a folder.
type PublishConfig struct {
	Folder        string `json:"folder"`
	ListenAddr    string `json:"listen_addr"`
	VirtualName   string `json:"virtual_name,omitempty"`
	FRPConnection string `json:"frp_connection,omitempty"` // hostname:port:password for FRP server
}

// PublishResult is the return value of Publish when FRP is used (Token is set).
type PublishResult struct {
	ID    string
	Token string // "id:secret" for mount client when using FRP; empty when not using FRP
}

// Publish starts a gRPC server exporting the given folder. Returns publish ID, token (if FRP), and error.
func (p *PublishManager) Publish(cfg PublishConfig) (result PublishResult, err error) {
	listenAddr := cfg.ListenAddr
	if listenAddr == "" {
		listenAddr = ":50051"
	}
	if cfg.FRPConnection != "" {
		listenAddr = "127.0.0.1:0"
	}
	virtualName := cfg.VirtualName
	if virtualName == "" {
		virtualName = "data"
	}
	paths := []*pb.ExportPath{
		{LocalPath: cfg.Folder, VirtualName: virtualName},
	}
	backend := export.NewBackend(paths)
	srv := source.NewServer(backend)

	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return PublishResult{}, err
	}
	var frpCancel func()
	if cfg.FRPConnection != "" {
		serverAddr, token, parseErr := frpclient.ParseFRPConnection(cfg.FRPConnection)
		if parseErr != nil {
			lis.Close()
			return PublishResult{}, parseErr
		}
		if checkErr := frpclient.CheckFRPServerReachable(serverAddr, 5*time.Second); checkErr != nil {
			lis.Close()
			return PublishResult{}, checkErr
		}
		common, cfgErr := frpclient.CommonConfig(serverAddr, token)
		if cfgErr != nil {
			lis.Close()
			return PublishResult{}, cfgErr
		}
		id, secret, genErr := frpclient.GenerateIDSecret()
		if genErr != nil {
			lis.Close()
			return PublishResult{}, genErr
		}
		localPort := lis.Addr().(*net.TCPAddr).Port
		frpCancel, err = frpclient.RunPublishProxies(context.Background(), common, id, secret, localPort)
		if err != nil {
			lis.Close()
			return PublishResult{}, err
		}
		result.Token = id + ":" + secret
	}

	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = source.RunContext(stopCh, lis, srv)
	}()

	stopFn := func() {
		close(stopCh)
		lis.Close()
		<-done
		if frpCancel != nil {
			frpCancel()
		}
	}

	p.mu.Lock()
	p.nextID++
	result.ID = fmt.Sprintf("publish-%d", p.nextID)
	p.publishes[result.ID] = &PublishEntry{
		ID:         result.ID,
		Folder:     cfg.Folder,
		ListenAddr: lis.Addr().String(),
		CreatedAt:  time.Now(),
		stop:       stopFn,
	}
	p.mu.Unlock()
	return result, nil
}

// Stop stops the publish server by ID.
func (p *PublishManager) Stop(id string) error {
	p.mu.Lock()
	ent, ok := p.publishes[id]
	if ok {
		delete(p.publishes, id)
	}
	p.mu.Unlock()
	if !ok {
		return ErrPublishNotFound
	}
	if ent.stop != nil {
		ent.stop()
	}
	return nil
}

// ListPublishes returns all active publishes.
func (p *PublishManager) ListPublishes() []PublishEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PublishEntry, 0, len(p.publishes))
	for _, e := range p.publishes {
		out = append(out, PublishEntry{ID: e.ID, Folder: e.Folder, ListenAddr: e.ListenAddr, CreatedAt: e.CreatedAt})
	}
	return out
}

// StopAll stops all publish servers.
func (p *PublishManager) StopAll() {
	p.mu.Lock()
	list := make([]*PublishEntry, 0, len(p.publishes))
	for _, e := range p.publishes {
		list = append(list, e)
	}
	p.publishes = make(map[string]*PublishEntry)
	p.mu.Unlock()
	for _, e := range list {
		if e.stop != nil {
			e.stop()
		}
	}
}
