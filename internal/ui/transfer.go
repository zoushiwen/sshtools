package ui

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"sshtools/config"
	fsutil "sshtools/internal/fs"
	sftpclient "sshtools/internal/sftp"
)

var errRetryLocalPath = errors.New("retry local path")

func (a *App) handleUpload() {
	machine, host, conn, err := a.openSFTPConnection("上传")
	if err != nil {
		fmt.Printf("上传准备失败: %v\n", err)
		return
	}
	defer conn.Close()

	localPath, err := a.readLine("请输入本地文件路径: ")
	if err != nil {
		fmt.Printf("读取本地路径失败: %v\n", err)
		return
	}
	if strings.TrimSpace(localPath) == "" {
		fmt.Println("本地路径不能为空")
		return
	}

	info, err := os.Stat(localPath)
	if err != nil {
		fmt.Printf("本地路径无效: %v\n", err)
		return
	}

	recursive := false
	if info.IsDir() {
		recursive, err = a.confirmYesNo("检测到本地路径是目录，是否递归上传？[y/N]: ", false)
		if err != nil {
			fmt.Printf("读取用户输入失败: %v\n", err)
			return
		}
		if !recursive {
			fmt.Println("已取消上传目录")
			return
		}
	}

	remoteTarget, err := a.readLine("请输入远程目标路径: ")
	if err != nil {
		fmt.Printf("读取远程路径失败: %v\n", err)
		return
	}
	if strings.TrimSpace(remoteTarget) == "" {
		fmt.Println("远程目标路径不能为空")
		return
	}

	fmt.Printf("开始上传到 %s (%s)\n", machine.Machine.Name, host)
	if info.IsDir() {
		files, total, err := fsutil.CollectLocalFiles(localPath)
		if err != nil {
			fmt.Printf("扫描本地目录失败: %v\n", err)
			return
		}

		remoteRoot, err := conn.ResolveUploadTarget(localPath, remoteTarget, true)
		if err != nil {
			fmt.Printf("解析远程目录失败: %v\n", err)
			return
		}

		if total == 0 {
			if err := conn.UploadDirectory(localPath, remoteRoot, files, nil); err != nil {
				fmt.Printf("上传目录失败: %v\n", err)
				return
			}
			fmt.Printf("目录为空，已创建远程目录: %s\n", remoteRoot)
			return
		}

		bar := sftpclient.NewProgressBar("上传中", total)
		if err := conn.UploadDirectory(localPath, remoteRoot, files, bar); err != nil {
			fmt.Printf("上传目录失败: %v\n", err)
			return
		}
		fmt.Printf("上传完成: %s\n", remoteRoot)
		return
	}

	remotePath, err := conn.ResolveUploadTarget(localPath, remoteTarget, false)
	if err != nil {
		fmt.Printf("解析远程文件路径失败: %v\n", err)
		return
	}

	bar := sftpclient.NewProgressBar("上传中", info.Size())
	if err := conn.UploadFile(localPath, remotePath, bar); err != nil {
		fmt.Printf("上传失败: %v\n", err)
		return
	}

	fmt.Printf("上传完成: %s\n", remotePath)
}

func (a *App) handleDownload() {
	machine, host, conn, err := a.openSFTPConnection("下载")
	if err != nil {
		fmt.Printf("下载准备失败: %v\n", err)
		return
	}
	defer conn.Close()

	remoteInput, err := a.readLine("请输入远程文件/目录路径: ")
	if err != nil {
		fmt.Printf("读取远程路径失败: %v\n", err)
		return
	}
	if strings.TrimSpace(remoteInput) == "" {
		fmt.Println("远程路径不能为空")
		return
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Printf("获取当前目录失败: %v\n", err)
		return
	}

	var plans []sftpclient.DownloadPlan
	var savedPaths []string
	for {
		localInput, readErr := a.readLine("请输入本地保存路径（直接回车使用当前目录）: ")
		if readErr != nil {
			fmt.Printf("读取本地路径失败: %v\n", readErr)
			return
		}

		plans, savedPaths, err = a.buildDownloadPlans(conn, cwd, remoteInput, localInput)
		if errors.Is(err, errRetryLocalPath) {
			fmt.Println("请重新输入本地保存路径")
			continue
		}
		if err != nil {
			fmt.Printf("准备下载任务失败: %v\n", err)
			return
		}
		break
	}

	if len(plans) == 0 {
		fmt.Println("目标目录为空，无需下载文件")
		for _, savedPath := range savedPaths {
			if err := os.MkdirAll(savedPath, 0o755); err != nil {
				fmt.Printf("创建本地目录失败: %v\n", err)
				return
			}
		}
		return
	}

	total := int64(0)
	for _, plan := range plans {
		total += plan.Size
	}

	fmt.Printf("开始从 %s (%s) 下载\n", machine.Machine.Name, host)
	bar := sftpclient.NewProgressBar("下载中", total)
	for _, plan := range plans {
		if err := conn.DownloadFile(plan.RemotePath, plan.LocalPath, bar); err != nil {
			fmt.Printf("下载失败: %v\n", err)
			return
		}
	}

	fmt.Println("下载完成，保存路径:")
	for _, savedPath := range savedPaths {
		fmt.Printf("- %s\n", savedPath)
	}
}

func (a *App) openSFTPConnection(action string) (machine config.IndexedMachine, host string, conn *sftpclient.Connection, err error) {
	hostInput, readErr := a.readLine(fmt.Sprintf("请输入用于%s的主机名或 ID: ", action))
	if readErr != nil {
		err = readErr
		return
	}

	machine, resolveErr := a.resolveMachineInput(hostInput)
	if resolveErr != nil {
		err = resolveErr
		return
	}

	host, err = a.selectHost(machine)
	if err != nil {
		return
	}

	conn, err = a.sftp.Connect(machine.Machine, host)
	if err != nil {
		return
	}

	return
}

func (a *App) buildDownloadPlans(conn *sftpclient.Connection, cwd, remoteInput, localInput string) ([]sftpclient.DownloadPlan, []string, error) {
	if fsutil.HasGlob(remoteInput) {
		return a.buildGlobDownloadPlans(conn, cwd, remoteInput, localInput)
	}

	info, err := conn.Stat(remoteInput)
	if err != nil {
		return nil, nil, fmt.Errorf("远程路径不存在或不可访问: %w", err)
	}

	if info.IsDir() {
		return a.buildDirectoryDownloadPlans(conn, cwd, remoteInput, localInput)
	}

	return a.buildSingleFileDownloadPlan(cwd, remoteInput, localInput, info.Size())
}

func (a *App) buildSingleFileDownloadPlan(cwd, remotePath, localInput string, size int64) ([]sftpclient.DownloadPlan, []string, error) {
	decision, err := fsutil.ResolveDownloadTarget(localInput, cwd, path.Base(remotePath), false)
	if err != nil {
		return nil, nil, err
	}

	if err := a.ensureLocalDirectory(decision.DirToEnsure); err != nil {
		return nil, nil, err
	}

	finalPath, cancelled, err := a.resolveExistingLocalFile(decision.FinalPath)
	if err != nil {
		return nil, nil, err
	}
	if cancelled {
		return nil, nil, errors.New("下载已取消")
	}

	return []sftpclient.DownloadPlan{{
		RemotePath: remotePath,
		LocalPath:  finalPath,
		Size:       size,
	}}, []string{finalPath}, nil
}

func (a *App) buildDirectoryDownloadPlans(conn *sftpclient.Connection, cwd, remotePath, localInput string) ([]sftpclient.DownloadPlan, []string, error) {
	rootName := path.Base(path.Clean(remotePath))
	decision, err := fsutil.ResolveDownloadTarget(localInput, cwd, rootName, true)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", errRetryLocalPath, err)
	}

	if err := a.ensureLocalDirectory(decision.DirToEnsure); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(decision.FinalPath, 0o755); err != nil {
		return nil, nil, fmt.Errorf("创建本地目录失败: %w", err)
	}

	files, _, err := conn.CollectRemoteFiles(remotePath)
	if err != nil {
		return nil, nil, fmt.Errorf("扫描远程目录失败: %w", err)
	}

	plans := make([]sftpclient.DownloadPlan, 0, len(files))
	for _, file := range files {
		targetPath := filepath.Join(decision.FinalPath, filepath.FromSlash(file.Rel))
		finalPath, cancelled, err := a.resolveExistingLocalFile(targetPath)
		if err != nil {
			return nil, nil, err
		}
		if cancelled {
			return nil, nil, errors.New("下载已取消")
		}

		plans = append(plans, sftpclient.DownloadPlan{
			RemotePath: file.Path,
			LocalPath:  finalPath,
			Size:       file.Size,
		})
	}

	return plans, []string{decision.FinalPath}, nil
}

func (a *App) buildGlobDownloadPlans(conn *sftpclient.Connection, cwd, remoteInput, localInput string) ([]sftpclient.DownloadPlan, []string, error) {
	matches, err := conn.Glob(remoteInput)
	if err != nil {
		return nil, nil, fmt.Errorf("展开通配符失败: %w", err)
	}
	if len(matches) == 0 {
		return nil, nil, fmt.Errorf("未匹配到任何远程文件")
	}

	baseDecision, err := fsutil.ResolveDirectoryBase(localInput, cwd)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", errRetryLocalPath, err)
	}
	if err := a.ensureLocalDirectory(baseDecision.DirToEnsure); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(baseDecision.FinalPath, 0o755); err != nil {
		return nil, nil, fmt.Errorf("创建本地目录失败: %w", err)
	}

	plans := make([]sftpclient.DownloadPlan, 0)
	savedPaths := make([]string, 0, len(matches))
	for _, match := range matches {
		info, err := conn.Stat(match)
		if err != nil {
			return nil, nil, err
		}

		if info.IsDir() {
			rootDir := filepath.Join(baseDecision.FinalPath, path.Base(path.Clean(match)))
			if err := os.MkdirAll(rootDir, 0o755); err != nil {
				return nil, nil, err
			}

			files, _, err := conn.CollectRemoteFiles(match)
			if err != nil {
				return nil, nil, err
			}

			for _, file := range files {
				targetPath := filepath.Join(rootDir, filepath.FromSlash(file.Rel))
				finalPath, cancelled, err := a.resolveExistingLocalFile(targetPath)
				if err != nil {
					return nil, nil, err
				}
				if cancelled {
					return nil, nil, errors.New("下载已取消")
				}

				plans = append(plans, sftpclient.DownloadPlan{
					RemotePath: file.Path,
					LocalPath:  finalPath,
					Size:       file.Size,
				})
			}
			savedPaths = append(savedPaths, rootDir)
			continue
		}

		targetPath := filepath.Join(baseDecision.FinalPath, path.Base(match))
		finalPath, cancelled, err := a.resolveExistingLocalFile(targetPath)
		if err != nil {
			return nil, nil, err
		}
		if cancelled {
			return nil, nil, errors.New("下载已取消")
		}

		plans = append(plans, sftpclient.DownloadPlan{
			RemotePath: match,
			LocalPath:  finalPath,
			Size:       info.Size(),
		})
		savedPaths = append(savedPaths, finalPath)
	}

	return plans, savedPaths, nil
}

func (a *App) ensureLocalDirectory(dir string) error {
	if strings.TrimSpace(dir) == "" || dir == "." {
		return nil
	}

	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%w: %s 不是目录", errRetryLocalPath, dir)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	create, err := a.confirmYesNo(fmt.Sprintf("目录不存在，是否创建？[y/N] %s: ", dir), false)
	if err != nil {
		return err
	}
	if !create {
		return fmt.Errorf("%w: 目录不存在: %s", errRetryLocalPath, dir)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	return nil
}

func (a *App) resolveExistingLocalFile(targetPath string) (string, bool, error) {
	if _, err := os.Stat(targetPath); err != nil {
		if os.IsNotExist(err) {
			return targetPath, false, nil
		}
		return "", false, err
	}

	for {
		choice, err := a.readLine(fmt.Sprintf("文件 %s 已存在，选择 [o]覆盖 [r]重命名 [c]取消: ", targetPath))
		if err != nil {
			return "", false, err
		}

		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "o":
			return targetPath, false, nil
		case "r":
			return fsutil.AutoRename(targetPath), false, nil
		case "c", "":
			return "", true, nil
		default:
			fmt.Println("请输入 o、r 或 c")
		}
	}
}

func (a *App) confirmYesNo(prompt string, defaultValue bool) (bool, error) {
	answer, err := a.readLine(prompt)
	if err != nil {
		return false, err
	}

	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	case "":
		return defaultValue, nil
	default:
		fmt.Println("请输入 y 或 n")
		return a.confirmYesNo(prompt, defaultValue)
	}
}
