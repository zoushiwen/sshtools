package sftpclient

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"github.com/schollz/progressbar/v3"
	"golang.org/x/crypto/ssh"

	"sshtools/config"
	fsutil "sshtools/internal/fs"
	sshconn "sshtools/internal/ssh"
)

type Client struct {
	connector *sshconn.Connector
}

type Connection struct {
	sshClient *ssh.Client
	client    *sftp.Client
}

type RemoteFile struct {
	Path string
	Rel  string
	Size int64
}

type DownloadPlan struct {
	RemotePath string
	LocalPath  string
	Size       int64
}

type progressWriter struct {
	file *os.File
	bar  *progressbar.ProgressBar
}

type progressReader struct {
	file *os.File
	bar  *progressbar.ProgressBar
}

func New(connector *sshconn.Connector) *Client {
	return &Client{connector: connector}
}

func (c *Client) Connect(machine config.Machine, host string) (*Connection, error) {
	sshClient, err := c.connector.Dial(machine, host)
	if err != nil {
		return nil, err
	}

	sftpClient, err := sftp.NewClient(
		sshClient,
		sftp.UseConcurrentReads(true),
	)
	if err != nil {
		_ = sshClient.Close()
		return nil, fmt.Errorf("创建 SFTP 客户端失败: %w", err)
	}

	return &Connection{
		sshClient: sshClient,
		client:    sftpClient,
	}, nil
}

func (c *Connection) Close() error {
	if c == nil {
		return nil
	}

	var sftpErr error
	if c.client != nil {
		sftpErr = c.client.Close()
	}

	var sshErr error
	if c.sshClient != nil {
		sshErr = c.sshClient.Close()
	}

	if sftpErr != nil {
		return sftpErr
	}
	return sshErr
}

func (c *Connection) Stat(remotePath string) (os.FileInfo, error) {
	return c.client.Stat(path.Clean(remotePath))
}

func (c *Connection) Glob(pattern string) ([]string, error) {
	return c.client.Glob(pattern)
}

func (c *Connection) ResolveUploadTarget(localPath, remoteTarget string, isDir bool) (string, error) {
	trimmed := strings.TrimSpace(remoteTarget)
	if trimmed == "" {
		return "", fmt.Errorf("远程路径不能为空")
	}

	cleaned := path.Clean(trimmed)
	remoteInfo, err := c.client.Stat(cleaned)
	if err == nil && remoteInfo.IsDir() {
		return path.Join(cleaned, filepath.Base(localPath)), nil
	}
	if err == nil && isDir {
		return "", fmt.Errorf("远程目标 %s 不是目录，无法上传目录", cleaned)
	}
	if err != nil && !isNotExist(err) {
		return "", err
	}

	if strings.HasSuffix(trimmed, "/") {
		return path.Join(cleaned, filepath.Base(localPath)), nil
	}

	return cleaned, nil
}

func (c *Connection) CollectRemoteFiles(remoteRoot string) ([]RemoteFile, int64, error) {
	cleanedRoot := path.Clean(remoteRoot)
	walker := c.client.Walk(cleanedRoot)
	files := make([]RemoteFile, 0)
	total := int64(0)

	for walker.Step() {
		if err := walker.Err(); err != nil {
			return nil, 0, err
		}
		if walker.Stat().IsDir() {
			continue
		}

		rel := strings.TrimPrefix(strings.TrimPrefix(walker.Path(), cleanedRoot), "/")
		if rel == "" {
			rel = path.Base(walker.Path())
		}

		files = append(files, RemoteFile{
			Path: walker.Path(),
			Rel:  rel,
			Size: walker.Stat().Size(),
		})
		total += walker.Stat().Size()
	}

	return files, total, nil
}

func (c *Connection) UploadFile(localPath, remotePath string, bar *progressbar.ProgressBar) error {
	if err := c.client.MkdirAll(path.Dir(remotePath)); err != nil {
		return fmt.Errorf("创建远程目录失败: %w", err)
	}

	localFile, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("打开本地文件失败: %w", err)
	}
	defer localFile.Close()

	remoteFile, err := c.client.Create(remotePath)
	if err != nil {
		return fmt.Errorf("创建远程文件失败: %w", err)
	}
	defer remoteFile.Close()

	reader := io.Reader(localFile)
	if bar != nil {
		reader = &progressReader{file: localFile, bar: bar}
	}

	// SFTP sequential writes are significantly slower than scp on higher-latency links.
	// Use the library's concurrent writer path explicitly to keep throughput competitive.
	written, err := remoteFile.ReadFromWithConcurrency(reader, 64)
	if err != nil {
		if truncateErr := remoteFile.Truncate(written); truncateErr != nil {
			return fmt.Errorf("上传文件失败: %w，截断远程文件失败: %v", err, truncateErr)
		}
		return fmt.Errorf("上传文件失败: %w", err)
	}
	if bar != nil {
		_ = bar.Finish()
	}

	return nil
}

func (c *Connection) UploadDirectory(localRoot, remoteRoot string, files []fsutil.LocalFile, bar *progressbar.ProgressBar) error {
	if err := c.client.MkdirAll(remoteRoot); err != nil {
		return fmt.Errorf("创建远程目录失败: %w", err)
	}

	for _, file := range files {
		remotePath := path.Join(remoteRoot, filepath.ToSlash(file.Rel))
		if err := c.UploadFile(file.Path, remotePath, bar); err != nil {
			return err
		}
	}

	return nil
}

func (c *Connection) DownloadFile(remotePath, localPath string, bar *progressbar.ProgressBar) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return fmt.Errorf("创建本地目录失败: %w", err)
	}

	remoteFile, err := c.client.Open(remotePath)
	if err != nil {
		return fmt.Errorf("打开远程文件失败: %w", err)
	}
	defer remoteFile.Close()

	localFile, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("创建本地文件失败: %w", err)
	}
	defer localFile.Close()

	writer := io.Writer(localFile)
	if bar != nil {
		writer = &progressWriter{file: localFile, bar: bar}
	}

	if _, err := remoteFile.WriteTo(writer); err != nil {
		return fmt.Errorf("下载文件失败: %w", err)
	}
	if bar != nil {
		_ = bar.Finish()
	}

	return nil
}

func NewProgressBar(description string, total int64) *progressbar.ProgressBar {
	return progressbar.NewOptions64(
		total,
		progressbar.OptionSetDescription(description),
		progressbar.OptionSetWidth(20),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionThrottle(100*time.Millisecond),
		progressbar.OptionOnCompletion(func() {
			fmt.Fprint(os.Stderr, "\n")
		}),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetPredictTime(false),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "=",
			SaucerHead:    ">",
			SaucerPadding: " ",
			BarStart:      "[",
			BarEnd:        "]",
		}),
	)
}

func isNotExist(err error) bool {
	return os.IsNotExist(err) || strings.Contains(strings.ToLower(err.Error()), "no such file")
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.file.Write(p)
	if n > 0 && w.bar != nil {
		_ = w.bar.Add64(int64(n))
	}
	return n, err
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.file.Read(p)
	if n > 0 && r.bar != nil {
		_ = r.bar.Add64(int64(n))
	}
	return n, err
}

func (r *progressReader) Stat() (os.FileInfo, error) {
	return r.file.Stat()
}
