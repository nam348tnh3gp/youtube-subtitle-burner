package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
	"sync"
	"bufio"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Cấu trúc request từ form
type BurnRequest struct {
	URL      string `json:"url"`
	Lang     string `json:"lang" binding:"required"` // ví dụ: "vi", "en"
	FontSize int    `json:"font_size" binding:"required,min=10,max=72"`
	Quality  string `json:"quality"` // "360" | "480" | "720" | "1080" | "best"
	Client   string `json:"client"` // tên trình duyệt cho cookies: "auto"|"firefox"|"chrome"...
	Player   string `json:"player"` // player_client yt-dlp: "auto"|"android_vr"|"web"|"ios"|"tv"|"mweb"
	// Nếu người dùng upload phụ đề có sẵn: tên file gốc + nội dung base64
	SubtitleName string `json:"subtitle_name"` // vd "my.vi.vtt" hoặc "my.srt"
	SubtitleData string `json:"subtitle_data"` // base64 nội dung file

// Cookies thủ công (ưu tiên hơn cookies-from-browser nếu có)
// "netscape": nội dung thô | "json": base64 mảng JSON (Cookie-Editor)
CookiesData   string `json:"cookies_data"`
CookiesFormat string `json:"cookies_format"`

	// Video upload (nếu có → bỏ qua tải YouTube)
	VideoName string `json:"video_name"` // vd "my-video.mp4"
	VideoData string `json:"video_data"` // base64 nội dung file
}

// Cấu trúc phụ đề trả về cho frontend
type SubtitleInfo struct {
	Language string `json:"language"`
	Name     string `json:"name"`
	URL      string `json:"url"`
}

// Cấu trúc response khi yêu cầu danh sách phụ đề
type SubtitleListResponse struct {
	Title     string         `json:"title"`
	Subtitles []SubtitleInfo `json:"subtitles"`
}

// Cấu trúc response sau khi xử lý xong
type BurnResponse struct {
	VideoURL string `json:"video_url"`
	Message  string `json:"message"`
}

func main() {
	r := gin.Default()

	// Giới hạn bộ nhớ multipart (dù chủ yếu dùng link)
	r.MaxMultipartMemory = 8 << 20 // 8 MiB

	// Phục vụ file tĩnh (video kết quả)
	r.Static("/tmp", "./tmp")

	// Load HTML template
	r.LoadHTMLGlob("templates/*")

	// Route giao diện
	r.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.html", nil)
	})

	// API: Lấy danh sách phụ đề có sẵn từ link YouTube
	r.POST("/api/subtitles", handleListSubtitles)

	// API: Tải video, tải phụ đề, ghép cứng và trả về link video
	r.POST("/api/burn", handleBurnSubtitles)

	// API: Lấy embed URL để preview video trên iframe
	r.POST("/api/preview", handlePreview)

// API: SSE stream tiến trình realtime
r.GET("/api/progress/:taskID", handleProgressSSE)

	// Chạy server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8988"
	}
	log.Printf("Server đang chạy tại http://localhost:%s", port)
	r.Run(":" + port)
}

// handleListSubtitles: Liệt kê các phụ đề có sẵn của video
func handleListSubtitles(c *gin.Context) {
	var req struct {
		URL string `json:"url" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "URL không hợp lệ"})
		return
	}

	// Tạo thư mục tạm nếu chưa có
	os.MkdirAll("./tmp", os.ModePerm)

	subs, title, err := listSubtitlesViaCLI(req.URL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể liệt kê phụ đề: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, SubtitleListResponse{
		Title:     title,
		Subtitles: subs,
	})
}

// listSubtitlesViaCLI gọi yt-dlp trực tiếp để lấy danh sách phụ đề.
// Trả về danh sách SubtitleInfo và tiêu đề video.
func listSubtitlesViaCLI(videoURL string) ([]SubtitleInfo, string, error) {
	// Lấy tiêu đề video
	titleCmd := exec.Command("yt-dlp", "--get-title", "--no-warnings", videoURL)
	titleBytes, err := titleCmd.Output()
	if err != nil {
		return nil, "", fmt.Errorf("không lấy được tiêu đề: %w", err)
	}
	title := strings.TrimSpace(string(titleBytes))

	// Lấy danh sách phụ đề (--list-subs)
	// Output dạng text, ta parse các dòng chứa mã ngôn ngữ.
	listCmd := exec.Command("yt-dlp", "--list-subs", "--no-warnings", videoURL)
	listBytes, err := listCmd.Output()
	if err != nil {
		return nil, title, fmt.Errorf("không lấy được danh sách phụ đề: %w", err)
	}

	// Parse output: tìm các dòng có dạng "vi  Vietnamese" hoặc tương tự.
	// yt-dlp list-subs thường in ra bảng với các cột: Language, Name, Formats...
	// Ta sẽ dùng cách đơn giản: tìm các dòng bắt đầu bằng mã ngôn ngữ (2-5 ký tự) theo sau là khoảng trắng.
	lines := strings.Split(string(listBytes), "\n")
	subs := []SubtitleInfo{}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "[") || strings.HasPrefix(line, "Language") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			langCode := fields[0]
			// Mã ngôn ngữ thường có 2-3 ký tự, có thể có dấu gạch nối (vd: pt-BR)
			if len(langCode) >= 2 && len(langCode) <= 6 {
				// Phần còn lại là tên hiển thị (có thể chứa khoảng trắng)
				name := strings.Join(fields[1:], " ")
				subs = append(subs, SubtitleInfo{
					Language: langCode,
					Name:     name,
					URL:      "", // không cần thiết ở bước này
				})
			}
		}
	}

	// Nếu không parse được, trả về ít nhất lựa chọn mặc định
	if len(subs) == 0 {
		subs = append(subs, SubtitleInfo{Language: "vi", Name: "Tiếng Việt"})
		subs = append(subs, SubtitleInfo{Language: "en", Name: "English"})
	}

	return subs, title, nil
}

// ============================================================
// PROGRESS TRACKER — theo dõi tiến trình từng task
// ============================================================
type TaskProgress struct {
Step     string  `json:"step"`     // "download" | "subtitle" | "convert" | "burn" | "done" | "error"
Percent  int     `json:"percent"`  // 0-100
Message  string  `json:"message"`
VideoURL string  `json:"video_url,omitempty"`
Error    string  `json:"error,omitempty"`
}

var (
progressMu sync.RWMutex
progressMap = map[string]*TaskProgress{}
)

func setProgress(taskID, step string, percent int, message string) {
progressMu.Lock()
defer progressMu.Unlock()
if p, ok := progressMap[taskID]; ok {
p.Step = step
p.Percent = percent
p.Message = message
}
}

func setProgressDone(taskID, videoURL string) {
progressMu.Lock()
defer progressMu.Unlock()
if p, ok := progressMap[taskID]; ok {
p.Step = "done"
p.Percent = 100
p.Message = "Done"
p.VideoURL = videoURL
}
}

func setProgressError(taskID, msg string) {
progressMu.Lock()
defer progressMu.Unlock()
if p, ok := progressMap[taskID]; ok {
p.Step = "error"
p.Message = msg
p.Error = msg
}
}

func getProgress(taskID string) TaskProgress {
progressMu.RLock()
defer progressMu.RUnlock()
if p, ok := progressMap[taskID]; ok {
return *p
}
return TaskProgress{Step: "unknown"}
}

// handleProgressSSE: SSE stream tiến trình realtime
func handleProgressSSE(c *gin.Context) {
taskID := c.Param("taskID")
if taskID == "" {
c.JSON(http.StatusBadRequest, gin.H{"error": "Missing taskID"})
return
}

c.Writer.Header().Set("Content-Type", "text/event-stream")
c.Writer.Header().Set("Cache-Control", "no-cache")
c.Writer.Header().Set("Connection", "keep-alive")
c.Writer.Header().Set("X-Accel-Buffering", "no")

flusher, ok := c.Writer.(http.Flusher)
if !ok {
c.JSON(http.StatusInternalServerError, gin.H{"error": "SSE not supported"})
return
}

ticker := time.NewTicker(500 * time.Millisecond)
defer ticker.Stop()

timeout := time.After(30 * time.Minute)

for {
select {
case <-c.Request.Context().Done():
return
case <-timeout:
fmt.Fprintf(c.Writer, "event: timeout\ndata: {}\n\n")
flusher.Flush()
return
case <-ticker.C:
p := getProgress(taskID)
data, _ := json.Marshal(p)
fmt.Fprintf(c.Writer, "data: %s\n\n", data)
flusher.Flush()
if p.Step == "done" || p.Step == "error" {
return
}
}
}
}

// handleBurnSubtitles: Tải video, tải phụ đề, chuyển sang ASS với font size, ghép cứng, trả về link.
func handleBurnSubtitles(c *gin.Context) {
var req BurnRequest
if err := c.ShouldBindJSON(&req); err != nil {
c.JSON(http.StatusBadRequest, gin.H{"error": "Dữ liệu không hợp lệ: " + err.Error()})
return
}
if strings.TrimSpace(req.URL) == "" && strings.TrimSpace(req.VideoData) == "" {
c.JSON(http.StatusBadRequest, gin.H{"error": "Cần nhập link YouTube hoặc upload video"})
return
}

taskID := uuid.New().String()

// Khởi tạo progress
progressMu.Lock()
progressMap[taskID] = &TaskProgress{Step: "queued", Percent: 0, Message: "Queued"}
progressMu.Unlock()

// Trả task_id NGAY để frontend mở SSE
c.JSON(http.StatusOK, gin.H{
"task_id": taskID,
"message": "Bắt đầu xử lý",
})

// Chạy nền
go processBurnTask(taskID, req)
}

// processBurnTask: chạy toàn bộ pipeline và cập nhật progress
func processBurnTask(taskID string, req BurnRequest) {
defer func() {
if r := recover(); r != nil {
setProgressError(taskID, fmt.Sprintf("panic: %v", r))
}
}()

workDir := filepath.Join("./tmp", taskID)
if err := os.MkdirAll(workDir, os.ModePerm); err != nil {
setProgressError(taskID, "Không tạo được thư mục tạm: "+err.Error())
return
}

videoPath := filepath.Join(workDir, "video.mp4")
subPath := filepath.Join(workDir, "subtitle."+req.Lang+".vtt")
assPath := filepath.Join(workDir, "subtitle.ass")
outputPath := filepath.Join(workDir, "output.mp4")

// ===== Bước 1: Video =====
setProgress(taskID, "download", 5, "Preparing video...")
if strings.TrimSpace(req.VideoData) != "" {
setProgress(taskID, "download", 10, "Decoding uploaded video...")
raw, err := base64.StdEncoding.DecodeString(req.VideoData)
if err != nil {
setProgressError(taskID, "Video base64 không hợp lệ: "+err.Error())
return
}
ext := filepath.Ext(req.VideoName)
if ext == "" {
ext = ".mp4"
}
videoPath = filepath.Join(workDir, "uploaded_video"+ext)
if err := os.WriteFile(videoPath, raw, 0644); err != nil {
setProgressError(taskID, "Không ghi được video upload: "+err.Error())
return
}
setProgress(taskID, "download", 30, fmt.Sprintf("Video ready (%d bytes)", len(raw)))
} else {
setProgress(taskID, "download", 10, "Downloading video from YouTube...")
formatSel := buildFormatSelector(req.Quality)
downloadCmd := exec.Command("yt-dlp",
"-f", formatSel,
"--merge-output-format", "mp4",
"-o", videoPath,
"--newline",
"--no-warnings",
req.URL,
)
downloadCmd.Stdout = os.Stdout
downloadCmd.Stderr = os.Stderr
if err := downloadCmd.Run(); err != nil {
setProgressError(taskID, "Tải video thất bại: "+err.Error())
return
}
setProgress(taskID, "download", 30, "Video downloaded")
}

// ===== Bước 2: Subtitle =====
setProgress(taskID, "subtitle", 35, "Preparing subtitles...")
if strings.TrimSpace(req.SubtitleData) != "" {
ext := ".vtt"
if strings.HasSuffix(strings.ToLower(req.SubtitleName), ".srt") {
ext = ".srt"
}
subPath = filepath.Join(workDir, "uploaded_subtitle"+ext)
raw, err := base64.StdEncoding.DecodeString(req.SubtitleData)
if err != nil {
setProgressError(taskID, "Subtitle base64 không hợp lệ: "+err.Error())
return
}
if err := os.WriteFile(subPath, raw, 0644); err != nil {
setProgressError(taskID, "Không ghi được subtitle upload: "+err.Error())
return
}
setProgress(taskID, "subtitle", 45, "Subtitle loaded (uploaded)")
} else {
setProgress(taskID, "subtitle", 40, "Downloading subtitles...")
langPattern := req.Lang
if !strings.Contains(langPattern, "*") {
langPattern = langPattern + ".*"
}
var cookiesFilePath string
if strings.TrimSpace(req.CookiesData) != "" {
cookiesFilePath = filepath.Join(workDir, "cookies.txt")
if err := writeCookiesFile(cookiesFilePath, req.CookiesData, req.CookiesFormat); err != nil {
setProgressError(taskID, "Cookies không hợp lệ: "+err.Error())
return
}
}
var errSub error
subPath, errSub = downloadSubtitlesWithCookies(
workDir, req.URL, langPattern, cookiesFilePath, req.Client, req.Player)
if errSub != nil {
setProgressError(taskID, "Tải phụ đề thất bại: "+errSub.Error())
return
}
setProgress(taskID, "subtitle", 55, "Subtitles downloaded")
}

// ===== Bước 3: Convert VTT → ASS =====
setProgress(taskID, "convert", 60, "Converting subtitles to ASS...")
convertCmd := exec.Command("ffmpeg",
"-y",
"-i", subPath,
assPath,
)
convertCmd.Stdout = nil
convertCmd.Stderr = nil
if err := convertCmd.Run(); err != nil {
log.Printf("[%s] Convert ASS thất bại, dùng VTT: %v", taskID, err)
assPath = subPath
setProgress(taskID, "convert", 65, "Using VTT directly")
} else {
setProgress(taskID, "convert", 70, "Converted to ASS")
}

// ===== Bước 4: Burn =====
setProgress(taskID, "burn", 72, "Burning subtitles into video...")
filter := fmt.Sprintf("subtitles='%s':force_style='FontSize=%d'",
escapePathForFFmpeg(assPath), req.FontSize)

burnCmd := exec.Command("ffmpeg",
"-y",
"-i", videoPath,
"-vf", filter,
"-c:v", "libx264",
"-preset", "fast",
"-crf", "23",
"-c:a", "aac",
"-b:a", "128k",
"-movflags", "+faststart",
"-progress", "pipe:1",
"-nostats",
outputPath,
)
// Đọc progress từ stderr của ffmpeg (chứa "time=HH:MM:SS")
stdoutPipe, err := burnCmd.StdoutPipe()
if err != nil {
setProgressError(taskID, "Không đọc được ffmpeg output: "+err.Error())
return
}
burnCmd.Stderr = nil
if err := burnCmd.Start(); err != nil {
setProgressError(taskID, "Không chạy được ffmpeg: "+err.Error())
return
}

// Đọc progress từ pipe
scanner := bufio.NewScanner(stdoutPipe)
lastPercent := 72
for scanner.Scan() {
line := scanner.Text()
// ffmpeg -progress xuất "out_time_ms=..." hoặc "out_time_us=..."
if strings.HasPrefix(line, "out_time_ms=") || strings.HasPrefix(line, "out_time_us=") {
val := strings.SplitN(line, "=", 2)
if len(val) == 2 {
if us, err := strconv.ParseInt(val[1], 10, 64); err == nil && us > 0 {
// Giả sử video dài tối đa 3600s (1h), map 0-100%
seconds := float64(us) / 1_000_000.0
percent := 72 + int(seconds/3600.0*28)
if percent > 99 {
percent = 99
}
if percent > lastPercent {
lastPercent = percent
setProgress(taskID, "burn", percent, fmt.Sprintf("Burning... %d%%", percent))
}
}
}
}
}

if err := burnCmd.Wait(); err != nil {
setProgressError(taskID, "Ghép phụ đề thất bại: "+err.Error())
return
}

videoURL := fmt.Sprintf("/tmp/%s/output.mp4", taskID)
setProgressDone(taskID, videoURL)
log.Printf("[%s] Hoàn tất: %s", taskID, videoURL)
}

// escapePathForFFmpeg xử lý đường dẫn để dùng trong filter của FFmpeg.
// FFmpeg yêu cầu escape dấu ':' và '\' trên Windows.
func escapePathForFFmpeg(path string) string {
	// Trên Unix, dấu ':' không cần escape trong filter? Thực tế cần escape dấu ':' vì nó phân tách filter.
	// Cách an toàn: thay '\' bằng '/', và escape ':'
	path = filepath.ToSlash(path)
	path = strings.ReplaceAll(path, ":", "\\:")
	// Escape dấu nháy đơn nếu có
	path = strings.ReplaceAll(path, "'", "'\\''")
	return path
}

// detectAvailableBrowsers: tự động dò các trình duyệt có cookie YouTube trên hệ thống.
// Trả về danh sách tên (theo thứ tự ưu tiên) và bỏ qua những trình duyệt không có dữ liệu cookies.
func detectAvailableBrowsers() []string {
	// Đường dẫn cookies đặc trưng của từng trình duyệt trên Linux/macOS
	home, _ := os.UserHomeDir()
	type candidate struct {
		name  string
		paths []string // đường dẫn tương đối từ home
	}
	cands := []candidate{
		{"firefox", []string{
			".mozilla/firefox",
			"Library/Application Support/Firefox",
			"AppData/Roaming/Mozilla/Firefox",
		}},
		{"chrome", []string{
			".config/google-chrome",
			"Library/Application Support/Google/Chrome",
			"AppData/Local/Google/Chrome/User Data",
		}},
		{"chromium", []string{
			".config/chromium",
			"Library/Application Support/Chromium",
		}},
		{"brave", []string{
			".config/BraveSoftware/Brave-Browser",
			"Library/Application Support/BraveSoftware/Brave-Browser",
			"AppData/Local/BraveSoftware/Brave-Browser/User Data",
		}},
		{"edge", []string{
			".config/microsoft-edge",
			"Library/Application Support/Microsoft Edge",
			"AppData/Local/Microsoft/Edge/User Data",
		}},
		{"opera", []string{
			".config/opera",
			"Library/Application Support/com.operasoftware.Opera",
		}},
		{"vivaldi", []string{
			".config/vivaldi",
			"Library/Application Support/Vivaldi",
		}},
	}

	var result []string
	for _, c := range cands {
		for _, rel := range c.paths {
			full := filepath.Join(home, rel)
			if st, err := os.Stat(full); err == nil && st.IsDir() {
				result = append(result, c.name)
				break
			}
		}
	}
	return result
}

// validPlayers: player_client yt-dlp chấp nhận (dùng cho --extractor-args).
var validPlayers = map[string]bool{
	"android_vr": true, "web": true, "web_safari": true, "web_embedded": true,
	"android": true, "ios": true, "tv": true, "mweb": true,
	"android_music": true, "web_music": true, "tv_embedded": true,
}

// normalizePlayer: chuẩn hoá giá trị player thành chuỗi player_client hợp lệ.
// Trả về "android_vr,web_safari" (ổn định nhất) nếu rỗng/auto/không hợp lệ.
func normalizePlayer(player string) string {
	p := strings.ToLower(strings.TrimSpace(player))
	if p == "" || p == "auto" {
		return "android_vr,web_safari"
	}
	// Nếu user chọn danh sách như "android_vr,web_safari"
	parts := strings.Split(p, ",")
	cleaned := []string{}
	for _, x := range parts {
		x = strings.TrimSpace(x)
		if validPlayers[x] {
			cleaned = append(cleaned, x)
		}
	}
	if len(cleaned) == 0 {
		return "android_vr,web_safari"
	}
	return strings.Join(cleaned, ",")
}

// validBrowsers: danh sách tên trình duyệt yt-dlp chấp nhận cho --cookies-from-browser.
var validBrowsers = map[string]bool{
	"brave": true, "chrome": true, "chromium": true, "edge": true,
	"firefox": true, "opera": true, "safari": true, "vivaldi": true, "whale": true,
}

// normalizeBrowser: chuẩn hoá chuỗi client thành tên trình duyệt hợp lệ.
// Trả về "" nếu giá trị không phải tên trình duyệt hợp lệ (=> dùng auto-detect).
func normalizeBrowser(client string) string {
	c := strings.ToLower(strings.TrimSpace(client))
	// Các giá trị cũ / không phải tên trình duyệt -> trả rỗng để auto-detect
	if c == "" || c == "auto" {
		return ""
	}
	// Nếu là danh sách (chứa dấu phẩy) -> không hợp lệ cho cookies
	if strings.Contains(c, ",") {
		return ""
	}
	if validBrowsers[c] {
		return c
	}
	return ""
}

// downloadSubtitles: tải phụ đề bằng yt-dlp với cookies trình duyệt.
// - Nếu preferredBrowser là tên trình duyệt hợp lệ: thử nó trước, sau đó auto-detect các trình duyệt khác.
// - Nếu preferredBrowser == "" hoặc "auto": chỉ auto-detect.
// - Không retry đa client (làm block 429 nặng hơn) — 1 lần gọi yt-dlp mỗi trình duyệt.
func downloadSubtitlesWithCookies(workDir, videoURL, langPattern, cookiesFilePath, preferredBrowser, playerClient string) (string, error) {
	// Xoá .vtt cũ
	oldMatches, _ := filepath.Glob(filepath.Join(workDir, "subtitle*.vtt"))
	for _, m := range oldMatches {
		os.Remove(m)
	}
// Nếu có cookies.txt thủ công → dùng ngay, không dò trình duyệt
if cookiesFilePath != "" {
log.Printf("  → Dùng cookies.txt: %s", cookiesFilePath)
err := runYtDlpSubtitles(workDir, videoURL, langPattern, playerClient, "--cookies", cookiesFilePath)
matches, _ := filepath.Glob(filepath.Join(workDir, "subtitle*.vtt"))
if len(matches) > 0 {
log.Printf("  ✓ Tải phụ đề thành công với cookies.txt")
return matches[0], nil
}
if err != nil {
if strings.Contains(err.Error(), "429") {
return "", fmt.Errorf("YouTube vẫn giới hạn IP (429) dù đã dùng cookies.txt. " +
"Cookies có thể hết hạn hoặc IP đã bị block cứng. Chờ 30-60 phút rồi thử lại")
}
return "", fmt.Errorf("tải phụ đề với cookies.txt thất bại: %w", err)
}
return "", fmt.Errorf("yt-dlp không tạo file .vtt từ cookies.txt")
}

	browsers := []string{}
	seen := map[string]bool{}
	add := func(b string) {
		b = strings.TrimSpace(b)
		if b != "" && validBrowsers[b] && !seen[b] {
			seen[b] = true
			browsers = append(browsers, b)
		}
	}

	// Ưu tiên trình duyệt user chọn (nếu hợp lệ)
	if nb := normalizeBrowser(preferredBrowser); nb != "" {
		add(nb)
	}
	// Tự dò các trình duyệt có dữ liệu trên máy
	for _, b := range detectAvailableBrowsers() {
		add(b)
	}

	if len(browsers) == 0 {
		return "", fmt.Errorf("không tìm thấy trình duyệt nào có cookies. " +
			"Vui lòng đăng nhập YouTube trên Firefox/Chrome/Edge/Brave trước khi thử lại")
	}

	log.Printf("  → Trình duyệt sẽ thử (theo thứ tự): %v", browsers)

	var lastErr error
	for _, b := range browsers {
		log.Printf("  → Thử cookies từ: %s", b)
		cmd := exec.Command("yt-dlp",
			"--cookies-from-browser", b,
			"--write-auto-subs",
			"--sub-langs", langPattern,
			"--sub-format", "vtt/best",
			"--skip-download",
			"--ignore-no-formats-error",
			"--extractor-args", "youtube:player_client="+normalizePlayer(playerClient),
			"-o", filepath.Join(workDir, "subtitle"),
			"--no-warnings",
			videoURL,
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()

		matches, _ := filepath.Glob(filepath.Join(workDir, "subtitle*.vtt"))
		if len(matches) > 0 {
			log.Printf("  ✓ Thành công với %s", b)
			return matches[0], nil
		}

		if err != nil {
			lastErr = err
			log.Printf("  → Cookies %s thất bại: %v", b, err)
			errStr := err.Error()
			if strings.Contains(errStr, "could not find") ||
				strings.Contains(errStr, "no such file") ||
				strings.Contains(errStr, "not found") ||
				strings.Contains(errStr, "database") {
				continue
			}
			if strings.Contains(errStr, "429") {
				return "", fmt.Errorf("YouTube vẫn giới hạn IP (429) dù đã dùng cookies từ %s. "+
					"Vui lòng thử lại sau 30-60 phút", b)
			}
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("không tải được phụ đề từ bất kỳ trình duyệt nào")
	}
	return "", fmt.Errorf("tải phụ đề thất bại: %w", lastErr)
}

// runYtDlpSubtitles: chạy yt-dlp tải phụ đề với 1 loại cookies.
// cookieFlag = "--cookies" (file) hoặc "--cookies-from-browser" (trình duyệt).
func runYtDlpSubtitles(workDir, videoURL, langPattern, playerClient, cookieFlag, cookieValue string) error {
args := []string{
cookieFlag, cookieValue,
"--write-auto-subs",
"--sub-langs", langPattern,
"--sub-format", "vtt/best",
"--skip-download",
"--ignore-no-formats-error",
"--extractor-args", "youtube:player_client=" + normalizePlayer(playerClient),
"-o", filepath.Join(workDir, "subtitle"),
"--no-warnings",
videoURL,
}
cmd := exec.Command("yt-dlp", args...)
cmd.Stdout = os.Stdout
cmd.Stderr = os.Stderr
return cmd.Run()
}

// writeCookiesFile: ghi cookies ra file Netscape để yt-dlp đọc qua --cookies.
func writeCookiesFile(path, data, format string) error {
format = strings.ToLower(strings.TrimSpace(format))
if format == "" {
format = "netscape"
}

var content string
switch format {
case "json":
raw := data
if decoded, err := base64.StdEncoding.DecodeString(data); err == nil {
raw = string(decoded)
}
ns, err := jsonCookiesToNetscape(raw)
if err != nil {
return fmt.Errorf("parse JSON cookies thất bại: %w", err)
}
content = ns
default:
content = data
if !strings.HasPrefix(content, "# Netscape HTTP Cookie File") &&
!strings.HasPrefix(content, "# HTTP Cookie File") {
content = "# Netscape HTTP Cookie File\n" + content
}
}

if err := os.WriteFile(path, []byte(content), 0644); err != nil {
return fmt.Errorf("không ghi được cookies.txt: %w", err)
}
return nil
}

// jsonCookiesToNetscape: convert bất kỳ định dạng JSON cookie nào sang Netscape.
// Hỗ trợ:
//  1) Mảng Cookie-Editor / EditThisCookie
//  2) Object wrapper: {"cookies":[...]}
//  3) Single object: {"name":..,"value":..,"domain":..}
//  4) Playwright: [{"name":..,"expires":..}]
func jsonCookiesToNetscape(jsonStr string) (string, error) {
jsonStr = strings.TrimSpace(jsonStr)

type Cookie struct {
Name     string      `json:"name"`
Value    string      `json:"value"`
Domain   string      `json:"domain"`
Path     string      `json:"path"`
URL      string      `json:"url"`
Expires  float64     `json:"expirationDate"`
Expires2 float64     `json:"expires"`
Expires3 interface{} `json:"expiry"`
Secure   bool        `json:"secure"`
HttpOnly bool        `json:"httpOnly"`
}

// Bước 1: thử parse []Cookie
var cookies []Cookie
if err := json.Unmarshal([]byte(jsonStr), &cookies); err != nil || len(cookies) == 0 {
// Bước 2: object wrapper
var wrapper struct {
Cookies []Cookie `json:"cookies"`
}
if err2 := json.Unmarshal([]byte(jsonStr), &wrapper); err2 == nil && len(wrapper.Cookies) > 0 {
cookies = wrapper.Cookies
} else {
// Bước 3: single object
var single Cookie
if err3 := json.Unmarshal([]byte(jsonStr), &single); err3 == nil && single.Name != "" {
cookies = []Cookie{single}
} else {
if err == nil {
err = fmt.Errorf("JSON rỗng hoặc không chứa cookie")
}
return "", fmt.Errorf("không nhận diện được cấu trúc JSON: %w", err)
}
}
}

var sb strings.Builder
sb.WriteString("# Netscape HTTP Cookie File\n")
sb.WriteString("# Generated from JSON cookies\n")
sb.WriteString("# https://curl.se/docs/http-cookies.html\n\n")

written := 0
for _, c := range cookies {
if c.Name == "" {
continue
}
domain := c.Domain
if domain == "" && c.URL != "" {
if u, err := url.Parse(c.URL); err == nil {
domain = u.Hostname()
if domain != "" && !strings.HasPrefix(domain, ".") {
domain = "." + domain
}
}
}
if domain == "" {
continue
}

includeSub := "FALSE"
if strings.HasPrefix(domain, ".") {
includeSub = "TRUE"
}

p := c.Path
if p == "" {
p = "/"
}

secure := "FALSE"
if c.Secure {
secure = "TRUE"
}

expires := int64(0)
if c.Expires > 0 {
expires = int64(c.Expires)
} else if c.Expires2 > 0 {
expires = int64(c.Expires2)
} else if c.Expires3 != nil {
switch v := c.Expires3.(type) {
case float64:
expires = int64(v)
case string:
if n, err := strconv.ParseInt(v, 10, 64); err == nil {
expires = n
}
}
}

// Netscape: domain TAB includeSub TAB path TAB secure TAB expires TAB name TAB value
sb.WriteString(fmt.Sprintf("%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
domain, includeSub, p, secure, expires, c.Name, c.Value))
written++
}

if written == 0 {
return "", fmt.Errorf("không có cookie hợp lệ nào sau khi convert")
}
return sb.String(), nil
}

// buildFormatSelector: map preset chất lượng sang format string của yt-dlp.
func buildFormatSelector(quality string) string {
	h := 0
	switch quality {
	case "360":
		h = 360
	case "480":
		h = 480
	case "720":
		h = 720
	case "1080":
		h = 1080
	}
	if h == 0 {
		return "bv*+ba/b"
	}
	// Chuỗi fallback linh hoạt — tránh lỗi "Requested format is not available"
	return fmt.Sprintf("bv*[height<=%d]+ba/b[height<=%d]/bv*+ba/b", h, h)
}

// extractYouTubeID: lấy video ID từ các dạng URL YouTube phổ biến.
func extractYouTubeID(raw string) string {
	raw = strings.TrimSpace(raw)
	patterns := []string{
		`youtu\.be/([A-Za-z0-9_-]{11})`,
		`youtube\.com/watch\?v=([A-Za-z0-9_-]{11})`,
		`youtube\.com/shorts/([A-Za-z0-9_-]{11})`,
		`youtube\.com/embed/([A-Za-z0-9_-]{11})`,
		`youtube\.com/live/([A-Za-z0-9_-]{11})`,
	}
	for _, p := range patterns {
		re := regexp.MustCompile(p)
		if m := re.FindStringSubmatch(raw); len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

// handlePreview: trả về embed URL để frontend hiển thị iframe xem trước.
func handlePreview(c *gin.Context) {
	var req struct {
		URL string `json:"url" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "URL không hợp lệ"})
		return
	}
	id := extractYouTubeID(req.URL)
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Không nhận diện được ID video YouTube"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"video_id":  id,
		"embed_url": "https://www.youtube.com/embed/" + id,
	})
}

