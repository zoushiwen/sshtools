package ui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/mattn/go-runewidth"
	"golang.org/x/term"

	"sshtools/config"
	sftpclient "sshtools/internal/sftp"
	sshconn "sshtools/internal/ssh"
)

const (
	machinePageSize     = 35
	commandHistoryLimit = 100
	greenText           = "\033[32m"
	resetText           = "\033[0m"
)

var ansiRegexp = regexp.MustCompile(`\x1b\[[0-9;]*m`)

type App struct {
	reader         *bufio.Reader
	commandHistory []string
	hostHistory    []string
	configPath     string
	cfg            *config.Config
	ssh            *sshconn.Connector
	sftp           *sftpclient.Client
}

func NewApp(configPath string) (*App, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}

	connector := sshconn.New(cfg.SSHTimeout)

	return &App{
		reader:     bufio.NewReader(os.Stdin),
		configPath: configPath,
		cfg:        cfg,
		ssh:        connector,
		sftp:       sftpclient.New(connector),
	}, nil
}

func (a *App) Run() error {
	a.printWelcome()

	for {
		input, err := a.readCommandLine("Opt> ", &a.commandHistory)
		if err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Println()
				return nil
			}
			return err
		}

		trimmed := strings.TrimSpace(input)
		if trimmed == "" {
			continue
		}
		appendHistory(&a.commandHistory, trimmed)

		fields := strings.Fields(trimmed)
		switch strings.ToLower(fields[0]) {
		case "":
			continue
		case "q":
			return nil
		case "?":
			a.printHelp()
		case "p":
			switch len(fields) {
			case 1:
				a.browseAllMachines()
			case 2:
				a.browseGroupMachines(fields[1])
			default:
				fmt.Println("用法: p 或 p <tag>")
			}
		case "g":
			if len(fields) > 1 {
				fmt.Println("用法: g")
				continue
			}
			a.printGroupOverview()
		case "r":
			if err := a.reloadConfig(); err != nil {
				fmt.Printf("重新加载配置失败: %v\n", err)
				continue
			}
			a.printConfigStatus("配置已重新加载")
		case "u":
			a.handleUpload()
		case "d":
			a.handleDownload()
		default:
			a.handleConnect(trimmed)
		}
	}
}

func (a *App) printWelcome() {
	a.renderMainMenu("")
}

func (a *App) printHelp() {
	lines := []string{
		fmt.Sprintf("1) %s    输入部分 IP / 主机名 搜索并登录（唯一匹配时直接连接）", colorizeGreen("Enter")),
		fmt.Sprintf("2) %s    输入 %s 前缀 + IP / 主机名 精确搜索", colorizeGreen("Enter"), colorizeGreen("/")),
		fmt.Sprintf("3) %s    输入 %s   显示所有主机列表", colorizeGreen("Enter"), colorizeGreen("p")),
		fmt.Sprintf("4) %s    输入 %s <tag> 只显示指定分组", colorizeGreen("Enter"), colorizeGreen("p")),
		fmt.Sprintf("5) %s    输入 %s   显示分组概览", colorizeGreen("Enter"), colorizeGreen("g")),
		fmt.Sprintf("6) %s    输入 %s   上传文件到指定主机", colorizeGreen("Enter"), colorizeGreen("u")),
		fmt.Sprintf("7) %s    输入 %s   从指定主机下载文件", colorizeGreen("Enter"), colorizeGreen("d")),
		fmt.Sprintf("8) %s    输入 %s   重新加载配置文件", colorizeGreen("Enter"), colorizeGreen("r")),
		fmt.Sprintf("9) %s    输入 %s   显示帮助", colorizeGreen("Enter"), colorizeGreen("?")),
		fmt.Sprintf("10) %s   输入 %s   退出", colorizeGreen("Enter"), colorizeGreen("q")),
	}

	for _, line := range lines {
		fmt.Printf("     %s\n", line)
	}
}

func (a *App) reloadConfig() error {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return err
	}

	a.cfg = cfg
	a.ssh = sshconn.New(cfg.SSHTimeout)
	a.sftp = sftpclient.New(a.ssh)

	return nil
}

func (a *App) handleConnect(input string) {
	if shouldClearBeforeFuzzyConnect(input) {
		clearScreen()
	}

	machine, err := a.resolveMachineInput(input)
	if err != nil {
		fmt.Printf("选择主机失败: %v\n", err)
		return
	}

	a.connectToMachine(machine)
}

func (a *App) connectToMachine(machine config.IndexedMachine) {
	host, err := a.selectHost(machine)
	if err != nil {
		fmt.Printf("选择连接地址失败: %v\n", err)
		return
	}

	fmt.Printf("正在连接 %s (%s)...\n", machine.Machine.Name, host)
	if err := a.ssh.StartInteractiveSession(machine.Machine, host); err != nil {
		a.resetInputReader()
		fmt.Printf("连接失败: %v\n", err)
		return
	}

	a.resetInputReader()
	a.renderMainMenu("已断开连接")
}

func (a *App) resolveMachineInput(input string) (config.IndexedMachine, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return config.IndexedMachine{}, fmt.Errorf("主机名或 ID 不能为空")
	}

	if id, err := strconv.Atoi(trimmed); err == nil {
		if machine, ok := a.cfg.FindByID(id); ok {
			return machine, nil
		}
	}

	exact := strings.HasPrefix(trimmed, "/")
	if exact {
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "/"))
		if trimmed == "" {
			return config.IndexedMachine{}, fmt.Errorf("精确搜索内容不能为空")
		}
	}

	matches := a.cfg.Search(trimmed, exact)
	if len(matches) == 0 {
		return config.IndexedMachine{}, fmt.Errorf("未找到匹配主机")
	}
	if len(matches) == 1 {
		return matches[0], nil
	}

	return a.selectMachineFromMatches(matches)
}

func (a *App) selectMachineFromMatches(matches []config.IndexedMachine) (config.IndexedMachine, error) {
	base := matches
	current := matches
	page := 1
	lastQuery := ""

	for {
		a.printSearchResultPage(current, page)
		a.printSearchPrompt(lastQuery)

		choice, err := a.readCommandLine("[Host]> ", &a.hostHistory)
		if err != nil {
			return config.IndexedMachine{}, err
		}

		trimmed := strings.TrimSpace(choice)
		lastQuery = trimmed
		appendHistory(&a.hostHistory, trimmed)

		switch strings.ToLower(trimmed) {
		case "":
			return config.IndexedMachine{}, fmt.Errorf("已取消选择")
		case "q":
			return config.IndexedMachine{}, fmt.Errorf("已取消选择")
		case "n":
			if page < calcPageCount(len(current), machinePageSize) {
				page++
				continue
			}
			fmt.Println("已经是最后一页")
			continue
		case "b":
			if page > 1 {
				page--
				continue
			}
			fmt.Println("已经是第一页")
			continue
		}

		if id, err := strconv.Atoi(trimmed); err == nil {
			if machine, ok := findMachineByDisplayID(current, id); ok {
				return machine, nil
			}
			fmt.Println("输入的 ID 不在当前匹配结果中，请重新输入")
			continue
		}

		switch {
		case strings.HasPrefix(trimmed, "//"):
			query := strings.TrimSpace(strings.TrimPrefix(trimmed, "//"))
			if query == "" {
				current = base
				page = 1
				continue
			}

			filtered := searchMachines(current, query, false)
			if len(filtered) == 0 {
				fmt.Println("没有资产")
				continue
			}
			if len(filtered) == 1 {
				return filtered[0], nil
			}
			current = filtered
			page = 1
		case strings.HasPrefix(trimmed, "/"):
			query := strings.TrimSpace(strings.TrimPrefix(trimmed, "/"))
			if query == "" {
				current = base
				page = 1
				continue
			}

			filtered := searchMachines(base, query, true)
			if len(filtered) == 0 {
				fmt.Println("没有资产")
				continue
			}
			if len(filtered) == 1 {
				return filtered[0], nil
			}
			current = filtered
			page = 1
		default:
			filtered := searchMachines(base, trimmed, false)
			if len(filtered) == 0 {
				fmt.Println("没有资产")
				continue
			}
			if len(filtered) == 1 {
				return filtered[0], nil
			}
			current = filtered
			page = 1
		}
	}
}

func (a *App) selectHost(machine config.IndexedMachine) (string, error) {
	publicIP := strings.TrimSpace(machine.Machine.PublicIP)
	intranetIP := strings.TrimSpace(machine.Machine.IntranetIP)

	switch {
	case publicIP == "" && intranetIP == "":
		return "", fmt.Errorf("主机 %s 未配置可用 IP", machine.Machine.Name)
	case publicIP == "":
		fmt.Printf("主机 %s 仅配置了内网 IP，将使用 %s\n", machine.Machine.Name, intranetIP)
		return intranetIP, nil
	case intranetIP == "":
		fmt.Printf("主机 %s 仅配置了外网 IP，将使用 %s\n", machine.Machine.Name, publicIP)
		return publicIP, nil
	}

	switch a.cfg.DefaultIPType {
	case config.IPSelectionPublic:
		fmt.Printf("主机 %s 按配置默认使用公网 IP: %s\n", machine.Machine.Name, publicIP)
		return publicIP, nil
	case config.IPSelectionIntranet:
		fmt.Printf("主机 %s 按配置默认使用内网 IP: %s\n", machine.Machine.Name, intranetIP)
		return intranetIP, nil
	}

	for {
		fmt.Printf("选择连接地址 [1] 外网 IP (%s) [2] 内网 IP (%s): ", publicIP, intranetIP)
		choice, err := a.readLine("")
		if err != nil && !errors.Is(err, io.EOF) {
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

func (a *App) browseAllMachines() {
	items := a.cfg.IndexedMachines()
	if len(items) == 0 {
		fmt.Println("暂无主机数据")
		return
	}

	a.browseMachineList("", items)
}

func (a *App) browseGroupMachines(tag string) {
	group, ok := a.cfg.FindGroupByTag(tag)
	if !ok {
		fmt.Printf("未找到分组 %q，输入 g 查看所有分组\n", tag)
		return
	}

	items, ok := a.cfg.IndexedMachinesByTag(tag)
	if !ok {
		fmt.Printf("未找到分组 %q，输入 g 查看所有分组\n", tag)
		return
	}

	a.browseMachineList(fmt.Sprintf("[%s / %s]", group.Name, group.Tag), items)
}

func (a *App) browseMachineList(title string, items []config.IndexedMachine) {
	base := items
	current := items
	page := 1
	lastQuery := ""
	needsRedraw := true
	for {
		if needsRedraw {
			a.renderMachineList(title, current, page, lastQuery)
		}

		input, err := a.readCommandLine("[Host]> ", &a.hostHistory)
		if err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Println()
				return
			}
			fmt.Printf("读取搜索条件失败: %v\n", err)
			return
		}

		trimmed := strings.TrimSpace(input)
		lastQuery = trimmed
		appendHistory(&a.hostHistory, trimmed)
		needsRedraw = false
		switch strings.ToLower(trimmed) {
		case "":
			return
		case "q":
			return
		case "n":
			if page < calcPageCount(len(current), machinePageSize) {
				page++
				needsRedraw = true
				continue
			}
			fmt.Println("已经是最后一页")
			continue
		case "b":
			if page > 1 {
				page--
				needsRedraw = true
				continue
			}
			fmt.Println("已经是第一页")
			continue
		}

		if id, err := strconv.Atoi(trimmed); err == nil {
			machine, ok := findMachineByDisplayID(current, id)
			if !ok {
				fmt.Println("未找到对应的资产 ID")
				continue
			}
			a.connectToMachine(machine)
			return
		}

		switch {
		case strings.HasPrefix(trimmed, "//"):
			query := strings.TrimSpace(strings.TrimPrefix(trimmed, "//"))
			if query == "" {
				current = base
				page = 1
				needsRedraw = true
				continue
			}

			matches := searchMachines(current, query, false)
			if len(matches) == 0 {
				fmt.Println("没有资产")
				continue
			}
			if len(matches) == 1 {
				a.connectToMachine(matches[0])
				return
			}
			current = matches
			page = 1
			needsRedraw = true
		case strings.HasPrefix(trimmed, "/"):
			query := strings.TrimSpace(strings.TrimPrefix(trimmed, "/"))
			if query == "" {
				current = base
				page = 1
				needsRedraw = true
				continue
			}

			matches := searchMachines(base, query, true)
			if len(matches) == 0 {
				fmt.Println("没有资产")
				continue
			}
			if len(matches) == 1 {
				a.connectToMachine(matches[0])
				return
			}
			current = matches
			page = 1
			needsRedraw = true
		default:
			matches := searchMachines(base, trimmed, false)
			if len(matches) == 0 {
				fmt.Println("没有资产")
				continue
			}
			if len(matches) == 1 {
				a.connectToMachine(matches[0])
				return
			}
			current = matches
			page = 1
			needsRedraw = true
		}
	}
}

func (a *App) renderMainMenu(status string) {
	clearScreen()
	fmt.Println("Welcome to SSH Tool")
	fmt.Println()
	a.printHelp()
	fmt.Println()
	a.printConfigStatus("")
	if strings.TrimSpace(status) != "" {
		fmt.Println(status)
	}
}

func (a *App) renderMachineList(title string, items []config.IndexedMachine, page int, query string) {
	clearScreen()
	if title != "" {
		fmt.Println(title)
	}
	a.printMachineTablePage(items, page)
	a.printSearchPrompt(query)
}

func (a *App) printGroupOverview() {
	summaries := a.cfg.GroupSummaries()
	if len(summaries) == 0 {
		fmt.Println("暂无分组数据")
		return
	}

	headers := []string{colorizeGreen("ID"), colorizeGreen("分组名称"), colorizeGreen("Tag"), colorizeGreen("主机数量")}
	rows := make([][]string, 0, len(summaries))
	for _, summary := range summaries {
		rows = append(rows, []string{
			strconv.Itoa(summary.ID),
			summary.Name,
			summary.Tag,
			strconv.Itoa(summary.MachineCount),
		})
	}

	renderMachineTable(headers, rows)
}

func (a *App) printMachineTable(items []config.IndexedMachine) {
	a.printMachineTablePage(items, 1)
}

func (a *App) printMachineTablePage(items []config.IndexedMachine, page int) {
	if len(items) == 0 {
		fmt.Println("没有资产")
		return
	}

	totalPages := calcPageCount(len(items), machinePageSize)
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}

	start := (page - 1) * machinePageSize
	end := start + machinePageSize
	if end > len(items) {
		end = len(items)
	}

	headers := []string{colorizeGreen("ID"), colorizeGreen("名称"), colorizeGreen("内网 IP"), colorizeGreen("外网 IP"), colorizeGreen("端口"), colorizeGreen("用户"), colorizeGreen("平台")}
	rows := make([][]string, 0, end-start)
	for offset, item := range items[start:end] {
		platform := strings.TrimSpace(item.Machine.Platform)
		if platform == "" {
			platform = "-"
		}

		rows = append(rows, []string{
			strconv.Itoa(start + offset + 1),
			item.Machine.Name,
			emptyFallback(item.Machine.IntranetIP),
			emptyFallback(item.Machine.PublicIP),
			strconv.Itoa(item.Machine.Port),
			item.Machine.User,
			platform,
		})
	}

	renderMachineTable(headers, rows)
	fmt.Println(colorizeGreen(fmt.Sprintf("页码：%d，每页行数：%d，总页数：%d，总数量：%d", page, machinePageSize, totalPages, len(items))))
	fmt.Println(colorizeGreen("提示：输入资产ID直接登录，二级搜索使用 // + 字段，如：//192 上一页：b 下一页：n"))
}

func (a *App) printSearchResultPage(items []config.IndexedMachine, page int) {
	if len(items) == 0 {
		fmt.Println("没有资产")
		return
	}

	totalPages := calcPageCount(len(items), machinePageSize)
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}

	start := (page - 1) * machinePageSize
	end := start + machinePageSize
	if end > len(items) {
		end = len(items)
	}

	headers := []string{colorizeGreen("ID"), colorizeGreen("名称"), colorizeGreen("分组"), colorizeGreen("内网 IP"), colorizeGreen("外网 IP")}
	rows := make([][]string, 0, end-start)
	for offset, item := range items[start:end] {
		rows = append(rows, []string{
			strconv.Itoa(start + offset + 1),
			item.Machine.Name,
			item.GroupTag,
			emptyFallback(item.Machine.IntranetIP),
			emptyFallback(item.Machine.PublicIP),
		})
	}

	fmt.Println("匹配结果：")
	renderMachineTable(headers, rows)
	fmt.Println(colorizeGreen(fmt.Sprintf("页码：%d，每页行数：%d，总页数：%d，总数量：%d", page, machinePageSize, totalPages, len(items))))
	fmt.Println(colorizeGreen("提示：输入资产ID直接登录，二级搜索使用 // + 字段，如：//192 上一页：b 下一页：n"))
}

func (a *App) readLine(prompt string) (string, error) {
	fmt.Print(prompt)

	input, err := a.reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}

	return strings.TrimSpace(input), err
}

func (a *App) readCommandLine(prompt string, history *[]string) (string, error) {
	if runtime.GOOS == "windows" || !term.IsTerminal(int(os.Stdin.Fd())) {
		return a.readLine(prompt)
	}

	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return a.readLine(prompt)
	}
	defer func() {
		_ = term.Restore(fd, oldState)
	}()
	defer a.resetInputReader()

	reader := bufio.NewReader(os.Stdin)
	current := []rune{}
	draft := ""
	historyPos := historyLength(history)

	redrawCommandLine(prompt, current)

	for {
		r, _, err := reader.ReadRune()
		if err != nil {
			return "", err
		}

		switch r {
		case '\r', '\n':
			fmt.Fprint(os.Stdout, "\r\n")
			return string(current), nil
		case 3:
			fmt.Fprint(os.Stdout, "^C\r\n")
			return "", nil
		case 4:
			if len(current) == 0 {
				return "", io.EOF
			}
		case 8, 127:
			if len(current) > 0 {
				current = current[:len(current)-1]
				redrawCommandLine(prompt, current)
			}
		case 27:
			next, err := reader.ReadByte()
			if err != nil {
				return "", err
			}
			if next != '[' {
				continue
			}

			action, err := reader.ReadByte()
			if err != nil {
				return "", err
			}

			switch action {
			case 'A':
				current, historyPos, draft = previousHistory(history, current, historyPos, draft)
				redrawCommandLine(prompt, current)
			case 'B':
				current, historyPos, draft = nextHistory(history, current, historyPos, draft)
				redrawCommandLine(prompt, current)
			}
		default:
			if r < 32 {
				continue
			}
			current = append(current, r)
			redrawCommandLine(prompt, current)
		}
	}
}

func historyLength(history *[]string) int {
	if history == nil {
		return 0
	}
	return len(*history)
}

func appendHistory(history *[]string, command string) {
	if history == nil {
		return
	}

	command = strings.TrimSpace(command)
	if command == "" {
		return
	}

	if len(*history) > 0 && (*history)[len(*history)-1] == command {
		return
	}

	*history = append(*history, command)
	if len(*history) > commandHistoryLimit {
		*history = append([]string(nil), (*history)[len(*history)-commandHistoryLimit:]...)
	}
}

func previousHistory(history *[]string, current []rune, historyPos int, draft string) ([]rune, int, string) {
	if history == nil || len(*history) == 0 {
		return current, historyPos, draft
	}

	items := *history
	if historyPos == len(items) {
		draft = string(current)
	}
	if historyPos <= 0 {
		return current, historyPos, draft
	}

	historyPos--
	return []rune(items[historyPos]), historyPos, draft
}

func nextHistory(history *[]string, current []rune, historyPos int, draft string) ([]rune, int, string) {
	if history == nil || len(*history) == 0 {
		return current, historyPos, draft
	}

	items := *history
	lastIndex := len(items) - 1
	if historyPos < lastIndex {
		historyPos++
		return []rune(items[historyPos]), historyPos, draft
	}
	if historyPos == lastIndex {
		return []rune(draft), len(items), draft
	}

	return current, historyPos, draft
}

func redrawCommandLine(prompt string, content []rune) {
	fmt.Fprintf(os.Stdout, "\r\033[K%s%s", prompt, string(content))
}

func emptyFallback(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	return value
}

func calcPageCount(totalItems, pageSize int) int {
	if totalItems <= 0 || pageSize <= 0 {
		return 1
	}

	pages := totalItems / pageSize
	if totalItems%pageSize != 0 {
		pages++
	}
	if pages == 0 {
		return 1
	}

	return pages
}

func colorizeGreen(value string) string {
	return greenText + value + resetText
}

func shouldClearBeforeFuzzyConnect(input string) bool {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "/") {
		return false
	}
	if _, err := strconv.Atoi(trimmed); err == nil {
		return false
	}

	return true
}

func clearScreen() {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("cmd", "/c", "cls")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err == nil {
			return
		}
	}

	fmt.Fprint(os.Stdout, "\033[H\033[2J")
}

func (a *App) resetInputReader() {
	a.reader = bufio.NewReader(os.Stdin)
}

func (a *App) printConfigStatus(prefix string) {
	if a.cfg.UsingEmbeddedDefault {
		if prefix == "" {
			fmt.Println("当前使用默认配置")
			return
		}
		fmt.Printf("%s: 当前使用默认配置\n", prefix)
		return
	}

	if prefix == "" {
		fmt.Printf("当前配置文件: %s\n", a.cfg.SourcePath)
		return
	}
	fmt.Printf("%s: %s\n", prefix, a.cfg.SourcePath)
}

func (a *App) printSearchPrompt(query string) {
	if strings.TrimSpace(query) == "" {
		fmt.Println(colorizeGreen("搜索："))
		return
	}

	fmt.Println(colorizeGreen("搜索：" + query))
}

func searchMachines(items []config.IndexedMachine, query string, exact bool) []config.IndexedMachine {
	normalized := strings.ToLower(strings.TrimSpace(query))
	if normalized == "" {
		return items
	}

	matches := make([]config.IndexedMachine, 0)
	for _, item := range items {
		values := []string{
			strings.ToLower(item.Machine.Name),
			strings.ToLower(item.Machine.IntranetIP),
			strings.ToLower(item.Machine.PublicIP),
			strings.ToLower(item.Machine.User),
			strings.ToLower(item.Machine.Platform),
			strconv.Itoa(item.Machine.Port),
			strings.ToLower(item.GroupName),
			strings.ToLower(item.GroupTag),
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

func findMachineByDisplayID(items []config.IndexedMachine, id int) (config.IndexedMachine, bool) {
	if id <= 0 || id > len(items) {
		return config.IndexedMachine{}, false
	}

	return items[id-1], true
}

func renderMachineTable(headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for idx, header := range headers {
		widths[idx] = visibleWidth(header)
	}

	for _, row := range rows {
		for idx, value := range row {
			if idx >= len(widths) {
				break
			}
			if width := visibleWidth(value); width > widths[idx] {
				widths[idx] = width
			}
		}
	}

	headerParts := make([]string, 0, len(headers))
	separatorParts := make([]string, 0, len(headers))
	for idx, header := range headers {
		headerParts = append(headerParts, formatMachineCell(header, widths[idx], alignLeft))
		separatorParts = append(separatorParts, strings.Repeat("-", widths[idx]+2))
	}

	fmt.Println(strings.Join(headerParts, "|"))
	fmt.Println(strings.Join(separatorParts, "+"))

	for _, row := range rows {
		lineParts := make([]string, 0, len(headers))
		for idx, value := range row {
			lineParts = append(lineParts, formatMachineCell(value, widths[idx], alignLeft))
		}
		fmt.Println(strings.Join(lineParts, "|"))
	}
}

const (
	alignLeft = iota
	alignRight
	alignCenter
)

func formatMachineCell(value string, width int, align int) string {
	displayWidth := visibleWidth(value)
	padding := width - displayWidth
	if padding < 0 {
		padding = 0
	}

	switch align {
	case alignRight:
		return " " + strings.Repeat(" ", padding) + value + " "
	case alignCenter:
		leftPadding := padding / 2
		rightPadding := padding - leftPadding
		return " " + strings.Repeat(" ", leftPadding) + value + strings.Repeat(" ", rightPadding) + " "
	default:
		return " " + value + strings.Repeat(" ", padding) + " "
	}
}

func visibleWidth(value string) int {
	return runewidth.StringWidth(ansiRegexp.ReplaceAllString(value, ""))
}
