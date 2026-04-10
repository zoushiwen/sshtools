package sshconn

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"sshtools/config"
)

type Connector struct {
	timeout time.Duration
}

func New(timeout time.Duration) *Connector {
	return &Connector{timeout: timeout}
}

func (c *Connector) Dial(machine config.Machine, host string) (*ssh.Client, error) {
	if host == "" {
		return nil, errors.New("未提供可连接的主机地址")
	}

	authMethods, err := buildAuthMethods(machine)
	if err != nil {
		return nil, err
	}

	sshConfig := &ssh.ClientConfig{
		User:            machine.User,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         c.timeout,
	}

	target := net.JoinHostPort(host, strconv.Itoa(machine.Port))
	client, err := ssh.Dial("tcp", target, sshConfig)
	if err != nil {
		return nil, fmt.Errorf("连接 %s 失败: %w", target, err)
	}

	return client, nil
}

func buildAuthMethods(machine config.Machine) ([]ssh.AuthMethod, error) {
	methods := make([]ssh.AuthMethod, 0, 2)

	if strings.TrimSpace(machine.PrivateKeyPath) != "" {
		authMethod, err := publicKeyAuthMethod(machine.PrivateKeyPath, machine.PrivateKeyPassphrase)
		if err != nil {
			return nil, fmt.Errorf("加载主机 %s 的私钥失败: %w", machine.Name, err)
		}
		methods = append(methods, authMethod)
	}

	if strings.TrimSpace(machine.Password) != "" {
		methods = append(methods, ssh.Password(machine.Password))
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("主机 %s 未配置可用的 SSH 认证方式", machine.Name)
	}

	return methods, nil
}

func publicKeyAuthMethod(privateKeyPath, passphrase string) (ssh.AuthMethod, error) {
	keyBytes, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("读取私钥文件失败: %w", err)
	}

	var signer ssh.Signer
	if strings.TrimSpace(passphrase) != "" {
		signer, err = ssh.ParsePrivateKeyWithPassphrase(keyBytes, []byte(passphrase))
	} else {
		signer, err = ssh.ParsePrivateKey(keyBytes)
	}
	if err != nil {
		var missingPassphrase *ssh.PassphraseMissingError
		if errors.As(err, &missingPassphrase) {
			return nil, errors.New("私钥已加密，请在配置中提供 private_key_passphrase")
		}
		return nil, fmt.Errorf("解析私钥失败: %w", err)
	}

	return ssh.PublicKeys(signer), nil
}

func (c *Connector) StartInteractiveSession(machine config.Machine, host string) error {
	client, err := c.Dial(machine, host)
	if err != nil {
		return err
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("创建 SSH 会话失败: %w", err)
	}
	defer session.Close()

	tty, err := openInteractiveTTY()
	if err != nil {
		return err
	}
	defer tty.Close()

	fd := int(tty.Fd())
	if !term.IsTerminal(fd) {
		return errors.New("当前终端不支持交互式 SSH 会话")
	}

	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("切换终端到 raw 模式失败: %w", err)
	}
	defer func() {
		_ = term.Restore(fd, oldState)
	}()

	width, height, err := term.GetSize(fd)
	if err != nil || width == 0 || height == 0 {
		width = 80
		height = 24
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", height, width, modes); err != nil {
		return fmt.Errorf("申请 PTY 失败: %w", err)
	}

	stdinPipe, err := session.StdinPipe()
	if err != nil {
		return fmt.Errorf("创建 SSH 输入管道失败: %w", err)
	}
	session.Stdout = os.Stdout
	session.Stderr = os.Stderr

	signals := make(chan os.Signal, 1)
	stopSignals := make(chan struct{})
	signal.Notify(signals, syscall.SIGWINCH)
	defer func() {
		close(stopSignals)
		signal.Stop(signals)
	}()

	go func() {
		for {
			select {
			case <-stopSignals:
				return
			case <-signals:
				w, h, sizeErr := term.GetSize(fd)
				if sizeErr == nil && w > 0 && h > 0 {
					_ = session.WindowChange(h, w)
				}
			}
		}
	}()
	signals <- syscall.SIGWINCH

	if err := session.Shell(); err != nil {
		return fmt.Errorf("启动远程 shell 失败: %w", err)
	}

	inputDone := make(chan struct{})
	inputErrCh := make(chan error, 1)
	go func() {
		inputErrCh <- copyInteractiveInput(fd, stdinPipe, inputDone)
	}()

	waitErr := session.Wait()
	close(inputDone)
	_ = stdinPipe.Close()
	inputErr := <-inputErrCh

	if waitErr == nil && inputErr != nil && !isExpectedInputError(inputErr) {
		return fmt.Errorf("本地终端输入异常: %w", inputErr)
	}

	if waitErr != nil {
		var exitError *ssh.ExitError
		if errors.As(waitErr, &exitError) {
			return nil
		}
		return fmt.Errorf("SSH 会话异常结束: %w", waitErr)
	}

	return nil
}

func openInteractiveTTY() (*os.File, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err == nil {
		return tty, nil
	}

	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		return nil, fmt.Errorf("打开控制终端失败: %w", err)
	}

	return nil, errors.New("当前终端不支持交互式 SSH 会话")
}

func copyInteractiveInput(fd int, writer io.WriteCloser, done <-chan struct{}) error {
	if err := unix.SetNonblock(fd, true); err != nil {
		return fmt.Errorf("设置终端为非阻塞模式失败: %w", err)
	}
	defer func() {
		_ = unix.SetNonblock(fd, false)
	}()

	buffer := make([]byte, 4096)
	for {
		select {
		case <-done:
			return nil
		default:
		}

		n, err := unix.Read(fd, buffer)
		if n > 0 {
			if _, writeErr := writer.Write(buffer[:n]); writeErr != nil {
				if isExpectedInputError(writeErr) {
					return nil
				}
				return writeErr
			}
		}

		if err == nil {
			continue
		}
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if isExpectedInputError(err) {
			return nil
		}

		return err
	}
}

func isExpectedInputError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
		return true
	}

	message := strings.ToLower(err.Error())
	return strings.Contains(message, "closed pipe") ||
		strings.Contains(message, "broken pipe") ||
		strings.Contains(message, "file already closed")
}
