package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- 类型定义 ---

type LyricData struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Lrc   string `json:"lrc"`
		Trans string `json:"trans"`
		Yrc   string `json:"yrc"`
		Roma  string `json:"roma"`
	} `json:"data"`
}

type WordInfo struct {
	Text      string
	StartTime int
	Duration  int
}

type LineInfo struct {
	Words     []WordInfo
	StartTime int
	EndTime   int
}

type DivInfo struct {
	StartTime int
	EndTime   int
	Lines     []*LineInfo
}

type MetaLine struct {
	Time    int
	Content string
}

type ErrorResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

// SearchSongItemRaw 用于解析上游API返回的原始歌曲条目
type SearchSongItemRaw struct {
	ID     int    `json:"id"`
	MID    string `json:"mid"`
	Song   string `json:"song"`
	Singer string `json:"singer"`
	Album  string `json:"album"`
}

// SearchSongItemSimplified 精简后的歌曲信息结构体
type SearchSongItemSimplified struct {
	N      int    `json:"n"`
	Song   string `json:"song"`
	Singer string `json:"singer"`
	ID     int    `json:"id"`
	MID    string `json:"mid"`
	Album  string `json:"album"`
}

// UnifiedLyricResponse 统一的歌词响应结构
type UnifiedLyricResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Song   string `json:"song"`
		Singer string `json:"singer"`
		Album  string `json:"album"`
		LRC    string `json:"lrc"`   // 原始 LRC (已合并翻译)
		ESLRC  string `json:"eslrc"` // 增强型 LRC (逐字)
		TTML   string `json:"ttml"`  // TTML 歌词
	} `json:"data"`
}

// SearchResponse 用于搜索结果的响应
type SearchResponse struct {
	Code    int                       `json:"code"`
	Message string                    `json:"message"`
	Data    []SearchSongItemSimplified `json:"data"`
}

// --- 全局变量和正则表达式 ---

var (
	yrcLineRe  = regexp.MustCompile(`^\[(\d+),(\d+)\](.*)$`)
	wordInfoRe = regexp.MustCompile(`(.*?)\((\d+),(\d+)\)`)
	lrcTimeRe  = regexp.MustCompile(`^\[(\d{2}):(\d{2})\.(\d{2,3})\](.*)$`)
	metaRe     = regexp.MustCompile(`^\[(ti|ar|al|by|offset|kana|re|ve):(.*?)\]$`)
)

var stringBuilderPool = sync.Pool{
	New: func() interface{} {
		return new(strings.Builder)
	},
}

var (
	debugMode = false
)

// --- 初始化 ---

func init() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	if os.Getenv("DEBUG") == "true" {
		debugMode = true
	}
}

func logDebug(format string, args ...interface{}) {
	if debugMode {
		log.Printf("[DEBUG] "+format, args...)
	}
}

func logInfo(format string, args ...interface{}) {
	log.Printf("[INFO] "+format, args...)
}

func logError(format string, args ...interface{}) {
	log.Printf("[ERROR] "+format, args...)
}

func getTTMLBuilder() *strings.Builder {
	sb := stringBuilderPool.Get().(*strings.Builder)
	sb.Reset()
	return sb
}

func putTTMLBuilder(sb *strings.Builder) {
	stringBuilderPool.Put(sb)
}

// --- 歌词解析和转换函数 ---

func parseLrcMeta(lrcContent string) map[string]string {
	meta := make(map[string]string)
	lines := strings.Split(lrcContent, "\n")
	for _, line := range lines {
		matches := metaRe.FindStringSubmatch(line)
		if len(matches) == 3 {
			meta[strings.TrimSpace(matches[1])] = strings.TrimSpace(matches[2])
		}
	}
	logDebug("解析到 %d 个元数据标签", len(meta))
	return meta
}

func parseYrcLine(line string) (*LineInfo, error) {
	matches := yrcLineRe.FindStringSubmatch(line)
	if len(matches) != 4 {
		return nil, fmt.Errorf("invalid YRC line format: %s", line)
	}

	startTime, _ := strconv.Atoi(matches[1])
	duration, _ := strconv.Atoi(matches[2])
	content := matches[3]

	lineInfo := &LineInfo{
		StartTime: startTime,
		EndTime:   startTime + duration,
	}

	wordMatches := wordInfoRe.FindAllStringSubmatch(content, -1)
	for _, match := range wordMatches {
		if len(match) == 4 {
			text := match[1]
			wordStartTime, _ := strconv.Atoi(match[2])
			wordDuration, _ := strconv.Atoi(match[3])

			if wordDuration == 0 {
				if strings.TrimSpace(text) != "" {
					wordDuration = 1
					logDebug("修正: 文本 '%s' (开始 %d) 持续时间为0，设为1ms", text, wordStartTime)
				} else {
					continue
				}
			}

			lineInfo.Words = append(lineInfo.Words, WordInfo{
				Text:      text,
				StartTime: wordStartTime,
				Duration:  wordDuration,
			})
		}
	}

	if len(lineInfo.Words) == 0 && content != "" {
		wordDuration := duration
		if wordDuration == 0 {
			wordDuration = 1
		}
		lineInfo.Words = append(lineInfo.Words, WordInfo{
			Text:      content,
			StartTime: startTime,
			Duration:  wordDuration,
		})
	}

	return lineInfo, nil
}

func parseYrcToLines(yrcContent string) []*LineInfo {
	var parsedLines []*LineInfo
	lines := strings.Split(yrcContent, "\n")

	for _, line := range lines {
		if !strings.HasPrefix(line, "[") || isMetadataLine(line) {
			continue
		}
		lineInfo, err := parseYrcLine(line)
		if err != nil {
			logError("解析YRC行失败: %v, 行内容: %s", err, line)
			continue
		}
		if len(lineInfo.Words) == 0 {
			continue
		}
		parsedLines = append(parsedLines, lineInfo)
	}
	return parsedLines
}

func parseLrcTimedLines(lrcContent string) []MetaLine {
	if strings.TrimSpace(lrcContent) == "" {
		return nil
	}

	var timedLines []MetaLine
	lines := strings.Split(lrcContent, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		matches := lrcTimeRe.FindStringSubmatch(line)
		if len(matches) == 5 {
			minutes, _ := strconv.Atoi(matches[1])
			seconds, _ := strconv.Atoi(matches[2])

			var milliseconds int
			msStr := matches[3]
			if len(msStr) == 2 {
				milliseconds, _ = strconv.Atoi(msStr)
				milliseconds *= 10
			} else {
				milliseconds, _ = strconv.Atoi(msStr)
			}

			totalMs := minutes*60*1000 + seconds*1000 + milliseconds
			content := strings.TrimSpace(matches[4])

			if content != "" && content != "//" && !strings.Contains(content, "QQ音乐") && !strings.Contains(content, "制作") {
				timedLines = append(timedLines, MetaLine{
					Time:    totalMs,
					Content: content,
				})
			}
		}
	}

	sort.Slice(timedLines, func(i, j int) bool {
		return timedLines[i].Time < timedLines[j].Time
	})

	return timedLines
}

// mergeLrcWithTranslation 合并原始LRC和翻译LRC
func mergeLrcWithTranslation(originalLrc, transLrc string) string {
	if strings.TrimSpace(transLrc) == "" {
		return originalLrc
	}

	var result strings.Builder
	translations := parseLrcTimedLines(transLrc)

	lines := strings.Split(originalLrc, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// 1. 写入原始行
		result.WriteString(line + "\n")

		// 2. 如果是歌词行，尝试查找并写入翻译
		if isMetadataLine(line) {
			continue
		}

		matches := lrcTimeRe.FindStringSubmatch(line)
		if len(matches) == 5 {
			minutes, _ := strconv.Atoi(matches[1])
			seconds, _ := strconv.Atoi(matches[2])
			var milliseconds int
			msStr := matches[3]
			if len(msStr) == 2 {
				milliseconds, _ = strconv.Atoi(msStr)
				milliseconds *= 10
			} else {
				milliseconds, _ = strconv.Atoi(msStr)
			}
			totalMs := minutes*60*1000 + seconds*1000 + milliseconds

			// 查找匹配的翻译
			transText := findClosestLine(totalMs, translations)
			if transText != "" {
				// 使用相同的时间戳格式写入翻译
				timestamp := msToLrcTime(totalMs)
				result.WriteString(fmt.Sprintf("%s%s\n", timestamp, transText))
			}
		}
	}

	return result.String()
}

func groupLinesIntoDivs(lines []*LineInfo, maxGap int) []DivInfo {
	if len(lines) == 0 {
		return nil
	}

	var divs []DivInfo
	currentDiv := DivInfo{
		StartTime: lines[0].StartTime,
		Lines:     []*LineInfo{lines[0]},
	}

	for i := 1; i < len(lines); i++ {
		prevLine := lines[i-1]
		var prevContentEndTime int
		if len(prevLine.Words) > 0 {
			lastWord := prevLine.Words[len(prevLine.Words)-1]
			prevContentEndTime = lastWord.StartTime + lastWord.Duration
		} else {
			prevContentEndTime = prevLine.EndTime
		}

		gap := lines[i].StartTime - prevContentEndTime

		if gap > maxGap {
			lastLineInCurrentDiv := currentDiv.Lines[len(currentDiv.Lines)-1]
			if len(lastLineInCurrentDiv.Words) > 0 {
				lastWord := lastLineInCurrentDiv.Words[len(lastLineInCurrentDiv.Words)-1]
				currentDiv.EndTime = lastWord.StartTime + lastWord.Duration
			} else {
				currentDiv.EndTime = lastLineInCurrentDiv.EndTime
			}
			divs = append(divs, currentDiv)

			currentDiv = DivInfo{
				StartTime: lines[i].StartTime,
				Lines:     []*LineInfo{lines[i]},
			}
		} else {
			currentDiv.Lines = append(currentDiv.Lines, lines[i])
		}
	}

	if len(currentDiv.Lines) > 0 {
		lastLineInCurrentDiv := currentDiv.Lines[len(currentDiv.Lines)-1]
		if len(lastLineInCurrentDiv.Words) > 0 {
			lastWord := lastLineInCurrentDiv.Words[len(lastLineInCurrentDiv.Words)-1]
			currentDiv.EndTime = lastWord.StartTime + lastWord.Duration
		} else {
			currentDiv.EndTime = lastLineInCurrentDiv.EndTime
		}
		divs = append(divs, currentDiv)
	}

	return divs
}

func calculateSongDuration(lines []*LineInfo) int {
	if len(lines) == 0 {
		return 0
	}

	maxEndTime := 0
	for _, line := range lines {
		if len(line.Words) > 0 {
			lastWord := line.Words[len(line.Words)-1]
			lineContentEndTime := lastWord.StartTime + lastWord.Duration
			if lineContentEndTime > maxEndTime {
				maxEndTime = lineContentEndTime
			}
		} else {
			if line.EndTime > maxEndTime {
				maxEndTime = line.EndTime
			}
		}
	}
	return maxEndTime + 1000
}

func matchRomajiLine(mainLineTime int, romajiLines []*LineInfo) *LineInfo {
	const maxTimeDiff = 100
	for _, romaLine := range romajiLines {
		timeDiff := abs(romaLine.StartTime-mainLineTime)
		if timeDiff <= maxTimeDiff {
			return romaLine
		}
	}
	return nil
}

func convertYrcToTtml(data *LyricData) (string, error) {
	sb := getTTMLBuilder()
	defer putTTMLBuilder(sb)

	translations := parseLrcTimedLines(data.Data.Trans)
	parsedLines := parseYrcToLines(data.Data.Yrc)
	parsedRomaji := parseYrcToLines(data.Data.Roma)

	if len(parsedLines) == 0 {
		return "", fmt.Errorf("未找到有效的YRC歌词行")
	}

	sb.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	sb.WriteString("<tt xmlns=\"http://www.w3.org/ns/ttml\" xmlns:ttm=\"http://www.w3.org/ns/ttml#metadata\" xmlns:itunes=\"http://music.apple.com/lyric-ttml-internal\" itunes:timing=\"Word\">\n")
	sb.WriteString("    <head>\n        <metadata>\n")
	sb.WriteString("            <ttm:agent type=\"person\" xml:id=\"v1\"/>\n")
	sb.WriteString("        </metadata>\n    </head>\n")

	songDuration := calculateSongDuration(parsedLines)
	songDurationStr := msToTtmlTime(songDuration)

	divs := groupLinesIntoDivs(parsedLines, 1000)

	sb.WriteString(fmt.Sprintf("    <body dur=\"%s\">\n", songDurationStr))

	lineCounter := 1
	for divIdx, div := range divs {
		divBegin := msToTtmlTime(div.StartTime)
		divEnd := msToTtmlTime(div.EndTime)

		sb.WriteString(fmt.Sprintf("        <div begin=\"%s\" end=\"%s\">\n", divBegin, divEnd))

		for _, line := range div.Lines {
			lineBegin := msToTtmlTime(line.StartTime)

			var pTagEndTime int
			if len(line.Words) > 0 {
				lastWord := line.Words[len(line.Words)-1]
				pTagEndTime = lastWord.StartTime + lastWord.Duration
			} else {
				pTagEndTime = line.EndTime
			}
			pTagEndTimeStr := msToTtmlTime(pTagEndTime)

			sb.WriteString(fmt.Sprintf("            <p begin=\"%s\" end=\"%s\" ttm:agent=\"v1\" itunes:key=\"L%d\">\n", lineBegin, pTagEndTimeStr, lineCounter))

			for _, word := range line.Words {
				wordBegin := msToTtmlTime(word.StartTime)
				wordEnd := msToTtmlTime(word.StartTime + word.Duration)
				sb.WriteString(fmt.Sprintf("                <span begin=\"%s\" end=\"%s\">%s</span>\n", wordBegin, wordEnd, word.Text))
			}

			transText := findClosestLine(line.StartTime, translations)
			if transText != "" {
				sb.WriteString(fmt.Sprintf("                <span ttm:role=\"x-translation\" xml:lang=\"zh-CN\">%s</span>\n", transText))
			}

			romaLine := matchRomajiLine(line.StartTime, parsedRomaji)
			if romaLine != nil {
				var romaBuilder strings.Builder
				hasContent := false
				for _, word := range romaLine.Words {
					trimmed := strings.TrimSpace(word.Text)
					if trimmed != "" {
						hasContent = true
						romaBuilder.WriteString(word.Text)
					}
				}
				if hasContent {
					romaText := strings.TrimSpace(romaBuilder.String())
					if romaText != "" {
						sb.WriteString(fmt.Sprintf("                <span ttm:role=\"x-roman\">%s</span>\n", romaText))
					}
				}
			}

			sb.WriteString("            </p>\n")
			lineCounter++
		}

		sb.WriteString("        </div>\n")
		if divIdx < len(divs)-1 {
			sb.WriteString("\n")
		}
	}

	sb.WriteString("    </body>\n</tt>\n")
	return sb.String(), nil
}

func convertYrcToEnhancedLrc(yrcContent, lrcContent, transContent, romaContent string) (string, error) {
	var result strings.Builder

	meta := parseLrcMeta(lrcContent)

	for key, value := range meta {
		if key != "kana" {
			result.WriteString(fmt.Sprintf("[%s:%s]\n", key, value))
		}
	}

	translations := parseLrcTimedLines(transContent)
	hasTranslation := len(translations) > 0

	rawLines := strings.Split(yrcContent, "\n")

	for _, line := range rawLines {
		line = strings.TrimSpace(line)
		if line == "" || isMetadataLine(line) {
			continue
		}

		lineInfo, err := parseYrcLine(line)
		if err != nil || len(lineInfo.Words) == 0 {
			continue
		}

		mainTimestamp := msToLrcTime(lineInfo.StartTime)
		result.WriteString(mainTimestamp)

		for _, word := range lineInfo.Words {
			wordTimestamp := msToEnhancedLrcTime(word.StartTime)
			result.WriteString(wordTimestamp)
			result.WriteString(word.Text)
		}

		if len(lineInfo.Words) > 0 {
			lastWord := lineInfo.Words[len(lineInfo.Words)-1]
			finalTimestamp := msToEnhancedLrcTime(lastWord.StartTime + lastWord.Duration)
			result.WriteString(finalTimestamp)
		}

		result.WriteString("\n")

		if hasTranslation {
			translation := findClosestLine(lineInfo.StartTime, translations)
			if translation != "" {
				result.WriteString(fmt.Sprintf("%s%s\n", mainTimestamp, translation))
			}
		}
	}

	return result.String(), nil
}

func findClosestLine(time int, lines []MetaLine) string {
	const maxTimeDiff = 500
	bestIndex := -1
	minDiff := maxTimeDiff

	for i, line := range lines {
		timeDiff := abs(line.Time - time)
		if timeDiff < minDiff {
			minDiff = timeDiff
			bestIndex = i
		}
	}

	if bestIndex != -1 {
		return lines[bestIndex].Content
	}
	return ""
}

func msToLrcTime(ms int) string {
	seconds := ms / 1000
	milliseconds := (ms % 1000) / 10
	minutes := seconds / 60
	seconds = seconds % 60
	return fmt.Sprintf("[%02d:%02d.%02d]", minutes, seconds, milliseconds)
}

func msToEnhancedLrcTime(ms int) string {
	seconds := ms / 1000
	milliseconds := (ms % 1000) / 10
	minutes := seconds / 60
	seconds = seconds % 60
	return fmt.Sprintf("<%02d:%02d.%02d>", minutes, seconds, milliseconds)
}

func msToTtmlTime(ms int) string {
	hours := ms / 3600000
	ms %= 3600000
	minutes := ms / 60000
	ms %= 60000
	seconds := ms / 1000
	milliseconds := ms % 1000

	if hours > 0 {
		return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, seconds, milliseconds)
	}
	return fmt.Sprintf("%02d:%02d.%03d", minutes, seconds, milliseconds)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func isMetadataLine(line string) bool {
	return strings.HasPrefix(line, "[ti:") ||
		strings.HasPrefix(line, "[ar:") ||
		strings.HasPrefix(line, "[al:") ||
		strings.HasPrefix(line, "[by:") ||
		strings.HasPrefix(line, "[offset:") ||
		strings.HasPrefix(line, "[kana:") ||
		strings.HasPrefix(line, "[re:") ||
		strings.HasPrefix(line, "[ve:")
}

// --- API 客户端函数 ---

const UPSTREAM_API_BASE = "https://api.vkeys.cn/v2/music/tencent"
const UPSTREAM_LYRIC_API = UPSTREAM_API_BASE + "/lyric"

// searchSongs 搜索歌曲
func searchSongs(word string, num int) ([]SearchSongItemSimplified, error) {
	searchURL := fmt.Sprintf("%s?word=%s&num=%d", UPSTREAM_API_BASE, url.QueryEscape(word), num)
	logInfo("搜索歌曲: %s (num=%d)", word, num)

	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(searchURL)
	if err != nil {
		return nil, fmt.Errorf("搜索请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取搜索响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("搜索API返回状态: %s", resp.Status)
	}

	var rawResult struct {
		Code    int                 `json:"code"`
		Message string              `json:"message"`
		Data    []SearchSongItemRaw `json:"data"`
	}

	if err := json.Unmarshal(body, &rawResult); err != nil {
		return nil, fmt.Errorf("解析搜索结果失败: %w", err)
	}

	if rawResult.Code != 200 {
		return nil, fmt.Errorf("搜索API返回错误: %s", rawResult.Message)
	}

	simplifiedSongs := make([]SearchSongItemSimplified, 0, len(rawResult.Data))
	for i, item := range rawResult.Data {
		simplifiedSongs = append(simplifiedSongs, SearchSongItemSimplified{
			N:      i + 1,
			Song:   item.Song,
			Singer: item.Singer,
			Album:  item.Album,
			ID:     item.ID,
			MID:    item.MID,
		})
	}

	return simplifiedSongs, nil
}

func fetchLyricData(id, mid string) (*LyricData, []byte, error) {
	var requestURL string
	if id != "" {
		requestURL = fmt.Sprintf("%s?id=%s", UPSTREAM_LYRIC_API, id)
	} else if mid != "" {
		requestURL = fmt.Sprintf("%s?mid=%s", UPSTREAM_LYRIC_API, mid)
	} else {
		return nil, nil, fmt.Errorf("ID 和 MID 均为空")
	}

	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(requestURL)
	if err != nil {
		return nil, nil, fmt.Errorf("上游歌词API请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("上游歌词API返回状态: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("读取歌词响应体失败: %w", err)
	}

	var lyricData LyricData
	if err := json.Unmarshal(body, &lyricData); err != nil {
		return nil, nil, fmt.Errorf("解析上游歌词JSON失败: %w", err)
	}

	return &lyricData, body, nil
}

// --- HTTP 处理函数 ---

// renderJSON 辅助函数：设置 Content-Type 并禁用 HTML 转义
func renderJSON(w http.ResponseWriter, statusCode int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.Encode(v)
}

func writeErrorJSON(w http.ResponseWriter, code int, message string, details string) {
	resp := ErrorResponse{
		Code:    code,
		Message: message,
		Details: details,
	}
	renderJSON(w, code, resp)
	logError("返回错误响应: [%d] %s - %s", code, message, details)
}

func lyricHandler(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	query := r.URL.Query()
	id := query.Get("id")
	mid := query.Get("mid")
	word := query.Get("word")
	nStr := query.Get("n")

	logInfo("收到请求: %s %s (ID=%s, MID=%s, Word=%s, n=%s)", r.Method, r.URL.Path, id, mid, word, nStr)

	// --- 辅助函数：构建统一的响应 ---
	buildResponse := func(song, singer, album string, data *LyricData) UnifiedLyricResponse {
		resp := UnifiedLyricResponse{
			Code:    200,
			Message: "请求成功",
		}
		resp.Data.Song = song
		resp.Data.Singer = singer
		resp.Data.Album = album

		// 1. 原始 LRC (合并翻译)
		resp.Data.LRC = mergeLrcWithTranslation(data.Data.Lrc, data.Data.Trans)

		// 2. 增强型 LRC (ESLRC) 和 TTML
		if data.Data.Yrc != "" {
			ttml, err := convertYrcToTtml(data)
			if err == nil {
				resp.Data.TTML = ttml
			} else {
				logError("TTML转换失败: %v", err)
			}

			eslrc, err := convertYrcToEnhancedLrc(data.Data.Yrc, data.Data.Lrc, data.Data.Trans, data.Data.Roma)
			if err == nil {
				resp.Data.ESLRC = eslrc
			} else {
				logError("增强LRC转换失败: %v", err)
			}
		}

		return resp
	}

	// --- 逻辑分支 1: 按关键字搜索 ---
	if word != "" {
		n, _ := strconv.Atoi(nStr)

		// Step 1: 搜索歌曲
		songs, err := searchSongs(word, 10)
		if err != nil {
			writeErrorJSON(w, http.StatusBadGateway, "搜索歌曲失败", err.Error())
			return
		}

		// Case 1: 仅搜索，不选择 (n=0 或 n 未提供)
		if n <= 0 {
			logInfo("返回 '%s' 的精简搜索结果", word)
			resp := SearchResponse{
				Code:    200,
				Message: "请求成功，请通过 n 参数选择歌曲获取歌词",
				Data:    songs,
			}
			renderJSON(w, http.StatusOK, resp)
			return
		}

		// Case 2: 搜索并选择第 n 首歌
		if len(songs) < n {
			writeErrorJSON(w, http.StatusNotFound, "歌曲索引超出范围", fmt.Sprintf("搜索 '%s' 只找到 %d 首歌", word, len(songs)))
			return
		}

		song := songs[n-1]
		logInfo("已选择第 %d 首歌: %s - %s", n, song.Song, song.Singer)

		// Step 2: 获取歌词数据
		data, _, err := fetchLyricData("", song.MID)
		if err != nil {
			writeErrorJSON(w, http.StatusBadGateway, "获取歌词失败", err.Error())
			return
		}

		if data.Code != 200 {
			writeErrorJSON(w, http.StatusNotFound, "未找到歌词", data.Message)
			return
		}

		// Step 3: 构建并发送响应
		resp := buildResponse(song.Song, song.Singer, song.Album, data)
		renderJSON(w, http.StatusOK, resp)
		logInfo("请求处理完成 (搜索+转换), 耗时: %v", time.Since(startTime))
		return
	}

	// --- 逻辑分支 2: 按 ID/MID 获取 ---
	if id != "" || mid != "" {
		data, rawJSON, err := fetchLyricData(id, mid)
		if err != nil {
			writeErrorJSON(w, http.StatusBadGateway, "获取上游数据失败", err.Error())
			return
		}

		if data.Code != 200 {
			logError("上游返回错误: Code=%d", data.Code)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusFailedDependency)
			w.Write(rawJSON)
			return
		}

		// 解析元数据填充歌曲信息
		meta := parseLrcMeta(data.Data.Lrc)
		songTitle := meta["ti"]
		singer := meta["ar"]
		album := meta["al"]

		resp := buildResponse(songTitle, singer, album, data)
		renderJSON(w, http.StatusOK, resp)
		logInfo("请求处理完成 (ID/MID转换), 耗时: %v", time.Since(startTime))
		return
	}

	// --- 逻辑分支 3: 参数错误 ---
	writeErrorJSON(w, http.StatusBadRequest, "缺少参数", "请提供 'id', 'mid' 或 'word' 参数")
}

// Handler 是 Vercel 的入口函数
func Handler(w http.ResponseWriter, r *http.Request) {
	// CORS 设置
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	lyricHandler(w, r)
}
