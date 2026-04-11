package ui

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"sshtools/config"
	fsutil "sshtools/internal/fs"
	sftpclient "sshtools/internal/sftp"
)

var (
	errRetryLocalPath    = errors.New("retry local path")
	errTransferCancelled = errors.New("transfer cancelled")
)

func (a *App) handleUpload() {
	machine, host, conn, err := a.openSFTPConnection()
	if err != nil {
		if errors.Is(err, errTransferCancelled) {
			return
		}
		fmt.Printf("上传准备失败: %v\n", err)
		return
	}
	defer conn.Close()

	localPath, info, recursive, err := a.promptUploadLocalSource()
	if err != nil {
		if errors.Is(err, errTransferCancelled) {
			return
		}
		fmt.Printf("读取本地路径失败: %v\n", err)
		return
	}

	remoteTarget, err := a.promptUploadRemoteTarget()
	if err != nil {
		if errors.Is(err, errTransferCancelled) {
			return
		}
		fmt.Printf("读取远程路径失败: %v\n", err)
		return
	}

	fmt.Printf("开始上传到 %s (%s)\n", machine.Machine.Name, host)
	if recursive {
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
			fmt.Println("上传完成！共上传 0 个文件，总大小 0B")
			return
		}

		bar := sftpclient.NewProgressBar("上传中", total)
		if err := conn.UploadDirectory(localPath, remoteRoot, files, bar); err != nil {
			fmt.Printf("上传目录失败: %v\n", err)
			return
		}
		fmt.Printf("上传完成！共上传 %d 个文件，总大小 %s\n", len(files), formatTransferSize(total))
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

	fmt.Printf("上传完成！共上传 1 个文件，总大小 %s\n", formatTransferSize(info.Size()))
}

func (a *App) handleDownload() {
	machine, host, conn, err := a.openSFTPConnection()
	if err != nil {
		if errors.Is(err, errTransferCancelled) {
			return
		}
		fmt.Printf("下载准备失败: %v\n", err)
		return
	}
	defer conn.Close()

	remoteInput, err := a.promptDownloadRemotePath(conn)
	if err != nil {
		if errors.Is(err, errTransferCancelled) {
			return
		}
		fmt.Printf("读取远程路径失败: %v\n", err)
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
		localInput, readErr := a.readLine("请输入本地保存路径（回车默认当前目录）> ")
		if readErr != nil {
			fmt.Printf("读取本地路径失败: %v\n", readErr)
			return
		}
		if strings.EqualFold(strings.TrimSpace(localInput), "q") {
			_ = a.cancelTransfer()
			return
		}

		plans, savedPaths, err = a.buildDownloadPlans(conn, cwd, remoteInput, localInput)
		if errors.Is(err, errRetryLocalPath) {
			fmt.Println("请重新输入本地保存路径")
			continue
		}
		if errors.Is(err, errTransferCancelled) {
			return
		}
		if err != nil {
			fmt.Printf("准备下载任务失败: %v\n", err)
			return
		}
		if strings.TrimSpace(localInput) == "" {
			a.printDownloadTargetPreview(cwd, savedPaths)
		}
		break
	}

	if len(plans) == 0 {
		for _, savedPath := range savedPaths {
			if err := os.MkdirAll(savedPath, 0o755); err != nil {
				fmt.Printf("创建本地目录失败: %v\n", err)
				return
			}
		}
		fmt.Println(buildDownloadStatus(cwd, savedPaths, 0, 0))
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

	fmt.Println(buildDownloadStatus(cwd, savedPaths, len(plans), total))
}

func (a *App) openSFTPConnection() (machine config.IndexedMachine, host string, conn *sftpclient.Connection, err error) {
	machine, err = a.selectTransferMachine()
	if err != nil {
		return
	}

	host, err = a.selectTransferHost(machine)
	if err != nil {
		return
	}

	conn, err = a.sftp.Connect(machine.Machine, host)
	if err != nil {
		return
	}

	return
}

func (a *App) selectTransferMachine() (config.IndexedMachine, error) {
	items := a.cfg.IndexedMachines()
	if len(items) == 0 {
		return config.IndexedMachine{}, fmt.Errorf("暂无主机数据")
	}

	a.renderTransferMachineList()
	return a.promptTransferMachineInput(items)
}

func (a *App) promptTransferMachineInput(items []config.IndexedMachine) (config.IndexedMachine, error) {
	current := items

	for {
		input, err := a.readCommandLine("请输入主机 ID、主机名或 IP> ", &a.hostHistory)
		if err != nil {
			return config.IndexedMachine{}, err
		}

		trimmed := strings.TrimSpace(input)
		appendHistory(&a.hostHistory, trimmed)

		switch {
		case trimmed == "":
			fmt.Println("输入不能为空，请重新输入")
			continue
		case strings.EqualFold(trimmed, "q"):
			return config.IndexedMachine{}, a.cancelTransfer()
		}

		if id, convErr := strconv.Atoi(trimmed); convErr == nil {
			machine, ok := a.cfg.FindByID(id)
			if !ok {
				fmt.Println("未找到匹配主机，请重新输入")
				continue
			}
			return machine, nil
		}

		exact := strings.HasPrefix(trimmed, "/")
		query := trimmed
		if exact {
			query = strings.TrimSpace(strings.TrimPrefix(trimmed, "/"))
			if query == "" {
				fmt.Println("输入不能为空，请重新输入")
				continue
			}
		}

		matches := searchTransferMachines(current, query, exact)
		switch len(matches) {
		case 0:
			fmt.Println("未找到匹配主机，请重新输入")
		case 1:
			return matches[0], nil
		default:
			current = matches
			a.renderTransferMachineMatches(matches)
			continue
		}
	}
}

func (a *App) selectTransferHost(machine config.IndexedMachine) (string, error) {
	publicIP := strings.TrimSpace(machine.Machine.PublicIP)
	intranetIP := strings.TrimSpace(machine.Machine.IntranetIP)

	switch {
	case publicIP == "" && intranetIP == "":
		return "", fmt.Errorf("主机 %s 未配置可用 IP", machine.Machine.Name)
	case publicIP == "":
		fmt.Printf("该主机未配置外网 IP，自动使用内网 IP: %s\n", intranetIP)
		return intranetIP, nil
	case intranetIP == "":
		fmt.Printf("该主机未配置内网 IP，自动使用外网 IP: %s\n", publicIP)
		return publicIP, nil
	}

	switch a.cfg.DefaultIPType {
	case config.IPSelectionPublic:
		fmt.Printf("使用外网 IP: %s\n", publicIP)
		return publicIP, nil
	case config.IPSelectionIntranet:
		fmt.Printf("使用内网 IP: %s\n", intranetIP)
		return intranetIP, nil
	}

	for {
		choice, err := a.readLine(fmt.Sprintf("使用 [1] 外网 IP(%s)  [2] 内网 IP(%s) > ", publicIP, intranetIP))
		if err != nil {
			return "", err
		}

		switch strings.TrimSpace(choice) {
		case "1":
			return publicIP, nil
		case "2":
			return intranetIP, nil
		default:
			fmt.Println("请输入 1 或 2")
		}
	}
}

func (a *App) promptUploadLocalSource() (string, os.FileInfo, bool, error) {
	for {
		localPath, err := a.readLine("请输入本地文件路径> ")
		if err != nil {
			return "", nil, false, err
		}

		switch {
		case strings.EqualFold(localPath, "q"):
			return "", nil, false, a.cancelTransfer()
		case strings.TrimSpace(localPath) == "":
			fmt.Println("路径不能为空，请重新输入")
			continue
		}

		info, err := os.Stat(localPath)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Println("本地路径不存在，请重新输入")
				continue
			}
			return "", nil, false, err
		}

		if !info.IsDir() {
			return localPath, info, false, nil
		}

		recursive, err := a.confirmYesNo("检测到目录，是否递归上传？[y/N] ", false)
		if err != nil {
			return "", nil, false, err
		}
		if !recursive {
			fmt.Println("请输入具体文件路径")
			continue
		}

		return localPath, info, true, nil
	}
}

func (a *App) promptUploadRemoteTarget() (string, error) {
	for {
		remoteTarget, err := a.readLine("请输入远程目标路径> ")
		if err != nil {
			return "", err
		}

		switch {
		case strings.EqualFold(remoteTarget, "q"):
			return "", a.cancelTransfer()
		case strings.TrimSpace(remoteTarget) == "":
			fmt.Println("路径不能为空，请重新输入")
			continue
		}

		return normalizeUploadRemoteTarget(remoteTarget), nil
	}
}

func (a *App) promptDownloadRemotePath(conn *sftpclient.Connection) (string, error) {
	for {
		remoteInput, err := a.readLine("请输入远程文件或目录路径> ")
		if err != nil {
			return "", err
		}

		switch {
		case strings.EqualFold(remoteInput, "q"):
			return "", a.cancelTransfer()
		case strings.TrimSpace(remoteInput) == "":
			fmt.Println("路径不能为空，请重新输入")
			continue
		}

		if fsutil.HasGlob(remoteInput) {
			matches, err := conn.Glob(remoteInput)
			if err != nil {
				fmt.Printf("展开通配符失败: %v\n", err)
				continue
			}
			if len(matches) == 0 {
				fmt.Println("未匹配到任何远程文件，请重新输入")
				continue
			}

			fmt.Println("匹配结果：")
			for _, match := range matches {
				fmt.Printf("- %s\n", match)
			}

			confirmed, err := a.confirmYesNo(fmt.Sprintf("共匹配 %d 个文件，确认下载？[y/N] ", len(matches)), false)
			if err != nil {
				return "", err
			}
			if !confirmed {
				return "", a.cancelTransfer()
			}

			return remoteInput, nil
		}

		info, err := conn.Stat(remoteInput)
		if err != nil {
			fmt.Println("远程路径不存在或不可访问，请重新输入")
			continue
		}
		if info.IsDir() {
			confirmed, err := a.confirmYesNo("检测到远程目录，是否递归下载？[y/N] ", false)
			if err != nil {
				return "", err
			}
			if !confirmed {
				return "", a.cancelTransfer()
			}
		}

		return remoteInput, nil
	}
}

func (a *App) renderTransferMachineList() {
	clearScreen()

	headers := []string{
		colorizeGreen("ID"),
		colorizeGreen("名称"),
		colorizeGreen("内网 IP"),
		colorizeGreen("外网 IP"),
		colorizeGreen("端口"),
		colorizeGreen("用户"),
	}

	nextID := 1
	for _, group := range a.cfg.Groups {
		if len(group.Machines) == 0 {
			continue
		}

		fmt.Printf("[%s / %s]\n", group.Name, group.Tag)
		rows := make([][]string, 0, len(group.Machines))
		for _, machine := range group.Machines {
			rows = append(rows, []string{
				strconv.Itoa(nextID),
				machine.Name,
				emptyFallback(machine.IntranetIP),
				emptyFallback(machine.PublicIP),
				strconv.Itoa(machine.Port),
				machine.User,
			})
			nextID++
		}
		renderMachineTable(headers, rows)
		fmt.Println()
	}
}

func (a *App) renderTransferMachineMatches(items []config.IndexedMachine) {
	clearScreen()
	fmt.Println("匹配结果：")

	headers := []string{
		colorizeGreen("ID"),
		colorizeGreen("名称"),
		colorizeGreen("分组"),
		colorizeGreen("内网 IP"),
		colorizeGreen("外网 IP"),
	}
	rows := make([][]string, 0, len(items))
	for _, item := range items {
		rows = append(rows, []string{
			strconv.Itoa(item.ID),
			item.Machine.Name,
			item.GroupTag,
			emptyFallback(item.Machine.IntranetIP),
			emptyFallback(item.Machine.PublicIP),
		})
	}

	renderMachineTable(headers, rows)
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
		return nil, nil, fmt.Errorf("%w: %v", errRetryLocalPath, err)
	}

	if err := a.ensureLocalDirectory(decision.DirToEnsure); err != nil {
		return nil, nil, err
	}

	finalPath, err := a.resolveExistingLocalFile(decision.FinalPath)
	if err != nil {
		return nil, nil, err
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
		finalPath, err := a.resolveExistingLocalFile(targetPath)
		if err != nil {
			return nil, nil, err
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
				finalPath, err := a.resolveExistingLocalFile(targetPath)
				if err != nil {
					return nil, nil, err
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
		finalPath, err := a.resolveExistingLocalFile(targetPath)
		if err != nil {
			return nil, nil, err
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

func (a *App) resolveExistingLocalFile(targetPath string) (string, error) {
	if _, err := os.Stat(targetPath); err != nil {
		if os.IsNotExist(err) {
			return targetPath, nil
		}
		return "", err
	}

	for {
		choice, err := a.readLine("文件已存在，是否覆盖？[y/N/r] ")
		if err != nil {
			return "", err
		}

		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "y", "yes":
			return targetPath, nil
		case "r":
			return fsutil.AutoRename(targetPath), nil
		case "", "n", "no":
			return "", a.cancelTransfer()
		default:
			fmt.Println("请输入 y、n 或 r")
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

func (a *App) cancelTransfer() error {
	a.renderMainMenu("")
	return errTransferCancelled
}

func (a *App) printDownloadTargetPreview(cwd string, savedPaths []string) {
	if len(savedPaths) == 0 {
		return
	}

	location := "./"
	if len(savedPaths) == 1 {
		location = displayLocalPath(cwd, savedPaths[0])
	}
	fmt.Printf("将保存至：%s\n", location)
}

func buildDownloadStatus(cwd string, savedPaths []string, fileCount int, total int64) string {
	location := "./"
	if len(savedPaths) == 1 {
		location = displayLocalPath(cwd, savedPaths[0])
	}
	return fmt.Sprintf("下载完成！保存至：%s  共下载 %d 个文件，总大小 %s", location, fileCount, formatTransferSize(total))
}

func searchTransferMachines(items []config.IndexedMachine, query string, exact bool) []config.IndexedMachine {
	normalized := strings.ToLower(strings.TrimSpace(query))
	if normalized == "" {
		return nil
	}

	matches := make([]config.IndexedMachine, 0)
	for _, item := range items {
		values := []string{
			strings.ToLower(item.Machine.Name),
			strings.ToLower(item.Machine.IntranetIP),
			strings.ToLower(item.Machine.PublicIP),
		}

		for _, value := range values {
			if value == "" {
				continue
			}

			if exact && value == normalized {
				matches = append(matches, item)
				break
			}
			if !exact && strings.Contains(value, normalized) {
				matches = append(matches, item)
				break
			}
		}
	}

	return matches
}

func normalizeUploadRemoteTarget(target string) string {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" || trimmed == "/" || strings.HasSuffix(trimmed, "/") {
		return trimmed
	}
	return trimmed + "/"
}

func displayLocalPath(cwd, target string) string {
	if strings.TrimSpace(target) == "" {
		return "./"
	}

	if rel, err := filepath.Rel(cwd, target); err == nil && rel != "" && rel != "." && !strings.HasPrefix(rel, "..") {
		return "." + string(os.PathSeparator) + rel
	}
	if target == cwd {
		return "./"
	}
	if filepath.IsAbs(target) {
		return target
	}
	if strings.HasPrefix(target, "."+string(os.PathSeparator)) || target == "." {
		return target
	}
	return "." + string(os.PathSeparator) + target
}

func formatTransferSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%dB", size)
	}

	div := int64(unit)
	exp := 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}

	suffixes := []string{"KB", "MB", "GB", "TB", "PB"}
	return fmt.Sprintf("%.1f%s", float64(size)/float64(div), suffixes[exp])
}
