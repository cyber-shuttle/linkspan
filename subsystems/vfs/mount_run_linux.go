//go:build linux

package vfs

import (
	"context"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/cyber-shuttle/linkspan/subsystems/vfs/cache"
	"github.com/cyber-shuttle/linkspan/subsystems/vfs/fileproto"
	"github.com/cyber-shuttle/linkspan/subsystems/vfs/frpclient"
	vfsmount "github.com/cyber-shuttle/linkspan/subsystems/vfs/mount"
	"github.com/cyber-shuttle/linkspan/subsystems/vfs/resolver"
	"github.com/cyber-shuttle/linkspan/subsystems/vfs/wire"
)

func doMount(cfg MountConfig) (unmount func() error, err error) {
	serverAddr := cfg.ServerAddr
	var frpCancel func()

	if cfg.Token != "" && cfg.FRPConnection != "" {
		parts := strings.SplitN(cfg.Token, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, errMountTokenInvalid
		}
		id, secret := parts[0], parts[1]
		server, authToken, parseErr := frpclient.ParseFRPConnection(cfg.FRPConnection)
		if parseErr != nil {
			return nil, parseErr
		}
		if checkErr := frpclient.CheckFRPServerReachable(server, 5*time.Second); checkErr != nil {
			return nil, checkErr
		}
		common, cfgErr := frpclient.CommonConfig(server, authToken)
		if cfgErr != nil {
			return nil, cfgErr
		}
		var addr string
		addr, frpCancel, err = frpclient.RunMountVisitors(context.Background(), common, id, secret)
		if err != nil {
			return nil, err
		}
		serverAddr = addr
	} else {
		serverAddr, err = resolver.ResolveServer(cfg.ServerAddr)
		if err != nil {
			return nil, err
		}
	}

	conn, err := net.Dial("tcp", serverAddr)
	if err != nil {
		return nil, err
	}
	wc := wire.NewConn(conn)
	fpClient := fileproto.NewClient(wc)
	go fpClient.Run()

	cacheSize := cfg.CacheSizeMB * 1024 * 1024
	if cacheSize <= 0 {
		cacheSize = 256 * 1024 * 1024
	}
	blockSize := cfg.BlockSizeKB * 1024
	if blockSize <= 0 {
		blockSize = 256 * 1024
	}
	metadataTTL := time.Duration(cfg.CacheTTLSec) * time.Second
	if metadataTTL <= 0 {
		metadataTTL = 30 * time.Second
	}
	cacheConfig := &cache.Config{
		MaxDataCacheSize:    cacheSize,
		BlockSize:           blockSize,
		DataTTL:             5 * time.Minute,
		MetadataTTL:         metadataTTL,
		DirectoryTTL:        metadataTTL,
		Enabled:             !cfg.NoCache,
		PrefetchBlocks:      4,
		MaxParallelFetches:  8,
		EnablePrefetch:      true,
		EnableParallelFetch: true,
		UseMmapCache:        cfg.MmapCache,
		MmapCacheDir:        cfg.CacheDir,
	}
	cachedClient := cache.NewCachedClient(fpClient, cacheConfig)
	root := &vfsmount.RemoteRoot{Client: fpClient, CachedClient: cachedClient}

	if cfg.CacheDir != "" {
		fileCacheSize := cfg.FileCacheMB * 1024 * 1024
		if fileCacheSize <= 0 {
			fileCacheSize = 1024 * 1024 * 1024
		}
		fileCache, err := cache.NewFileCache(cfg.CacheDir, fileCacheSize)
		if err != nil {
			cachedClient.Close()
			conn.Close()
			return nil, err
		}
		root.FileCache = fileCache
		root.Passthrough = cfg.Passthrough
	}

	sec := time.Second
	opts := &fs.Options{
		AttrTimeout:  &sec,
		EntryTimeout: &sec,
		MountOptions: fuse.MountOptions{AllowOther: cfg.AllowOther},
	}
	server, err := fs.Mount(cfg.Mountpoint, root, opts)
	if err != nil {
		if root.FileCache != nil {
			root.FileCache.Close()
		}
		cachedClient.Close()
		conn.Close()
		return nil, err
	}
	waitDone := make(chan struct{})
	go func() {
		server.Wait()
		close(waitDone)
	}()

	var once sync.Once
	mp := cfg.Mountpoint
	unmount = func() error {
		var err error
		once.Do(func() {
			fpClient.Close()
			conn.Close()
			_ = server.Unmount()
			select {
			case <-waitDone:
			case <-time.After(10 * time.Second):
				for _, name := range []string{"fusermount", "fusermount3"} {
					if e := exec.Command(name, "-u", mp).Run(); e == nil {
						break
					}
				}
			}
			if root.FileCache != nil {
				root.FileCache.Close()
			}
			cachedClient.Close()
			if frpCancel != nil {
				frpCancel()
			}
		})
		return err
	}
	return unmount, nil
}
