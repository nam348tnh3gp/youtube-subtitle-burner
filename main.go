package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	URL      string `json:"url" binding:"required"`
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

// handleBurnSubtitles: Tải video, tải phụ đề, chuyển sang ASS với font size, ghép cứng, trả về link.
func handleBurnSubtitles(c *gin.Context) {
	var req BurnRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Dữ liệu không hợp lệ: " + err.Error()})
		return
	}

	// Tạo ID duy nhất cho tác vụ
	taskID := uuid.New().String()
	workDir := filepath.Join("./tmp", taskID)
	if err := os.MkdirAll(workDir, os.ModePerm); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không tạo được thư mục tạm"})
		return
	}

	// Đường dẫn file
	videoPath := filepath.Join(workDir, "video.mp4")
	subPath := filepath.Join(workDir, "subtitle."+req.Lang+".vtt") // yt-dlp mặc định tải VTT
	assPath := filepath.Join(workDir, "subtitle.ass")
	outputPath := filepath.Join(workDir, "output.mp4")

	// Bước 1: Tải video
	log.Printf("[%s] Đang tải video: %s", taskID, req.URL)
	formatSel := buildFormatSelector(req.Quality)
	log.Printf("[%s] Format selector: %s", taskID, formatSel)
	downloadCmd := exec.Command("yt-dlp",
		"-f", formatSel,
		"--merge-output-format", "mp4",
		"-o", videoPath,
		"--no-warnings",
		req.URL,
	)
	downloadCmd.Stdout = os.Stdout
	downloadCmd.Stderr = os.Stderr
	if err := downloadCmd.Run(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Tải video thất bại: " + err.Error()})
		return
	}

	// Bước 2: Lấy phụ đề — ưu tiên file upload nếu có, ngược lại tải từ YouTube
	if strings.TrimSpace(req.SubtitleData) != "" {
		// Người dùng import phụ đề có sẵn
		ext := ".vtt"
		if strings.HasSuffix(strings.ToLower(req.SubtitleName), ".srt") {
			ext = ".srt"
		}
		subPath = filepath.Join(workDir, "uploaded_subtitle"+ext)
		raw, err := base64.StdEncoding.DecodeString(req.SubtitleData)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Dữ liệu phụ đề base64 không hợp lệ: " + err.Error()})
			return
		}
		if err := os.WriteFile(subPath, raw, 0644); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Không ghi được file phụ đề upload"})
			return
		}
		log.Printf("[%s] Dùng phụ đề upload: %s (%d bytes)", taskID, subPath, len(raw))
} else {
// Tải từ YouTube: ưu tiên cookies thủ công nếu có
langPattern := req.Lang
if !strings.Contains(langPattern, "*") {
langPattern = langPattern + ".*"
}

var cookiesFilePath string
if strings.TrimSpace(req.CookiesData) != "" {
cookiesFilePath = filepath.Join(workDir, "cookies.txt")
if err := writeCookiesFile(cookiesFilePath, req.CookiesData, req.CookiesFormat); err != nil {
c.JSON(http.StatusBadRequest, gin.H{"error": "Cookies không hợp lệ: " + err.Error()})
return
}
log.Printf("[%s] Dùng cookies thủ công (format=%s): %s",
taskID, req.CookiesFormat, cookiesFilePath)
} else {
log.Printf("[%s] Tải phụ đề: %s (browser=%s, player=%s)",
taskID, req.Lang, req.Client, req.Player)
}

var errSub error
subPath, errSub = downloadSubtitlesWithCookies(
workDir, req.URL, langPattern, cookiesFilePath, req.Client, req.Player)
if errSub != nil {
c.JSON(http.StatusInternalServerError, gin.H{"error": "Tải phụ đề thất bại: " + errSub.Error()})
return
}
}
	log.Printf("[%s] File phụ đề: %s", taskID, subPath)

	// Bước 3: Chuyển VTT sang ASS với font size tùy chỉnh
	// Sử dụng FFmpeg để convert, đồng thời chèn style với FontSize mong muốn.
	// Cách đơn giản: dùng filter subtitles với force_style.
	// Tuy nhiên force_style không phải lúc nào cũng hoạt động tốt với ASS.
	// Cách ổn định hơn: tạo file ASS trực tiếp bằng cách chuyển đổi qua ffmpeg,
	// sau đó sửa header ASS để đặt PlayRes và FontSize.
	// Ở đây, để đơn giản và hiệu quả, ta dùng ffmpeg convert VTT -> ASS,
	// rồi dùng filter subtitles với force_style khi burn.
	// force_style có thể set FontSize.
	log.Printf("[%s] Đang chuyển đổi phụ đề sang ASS...", taskID)

	// Chuyển VTT sang ASS bằng ffmpeg (không burn, chỉ convert)
	convertCmd := exec.Command("ffmpeg",
		"-i", subPath,
		assPath,
	)
	convertCmd.Stdout = os.Stdout
	convertCmd.Stderr = os.Stderr
	if err := convertCmd.Run(); err != nil {
		// Nếu convert thất bại, thử dùng trực tiếp file VTT với force_style
		log.Printf("[%s] Convert ASS thất bại, sẽ dùng VTT trực tiếp: %v", taskID, err)
		assPath = subPath // fallback dùng VTT
	}

	// Bước 4: Ghép cứng phụ đề vào video với font size tùy chỉnh
	// Sử dụng filter "subtitles" với force_style để set FontSize.
	// FFmpeg filter: subtitles=filename:force_style='FontSize=24'
	// Cần escape đường dẫn đúng cách.
	log.Printf("[%s] Đang ghép cứng phụ đề (FontSize=%d)...", taskID, req.FontSize)

	// Xây dựng filter string
	// Đối với ASS, force_style có thể override FontSize.
	// Đối với VTT, ffmpeg sẽ render với style mặc định, force_style cũng có tác dụng.
	filter := fmt.Sprintf("subtitles='%s':force_style='FontSize=%d'",
		escapePathForFFmpeg(assPath), req.FontSize)

	burnCmd := exec.Command("ffmpeg",
		"-i", videoPath,
		"-vf", filter,
		"-c:v", "libx264",
		"-preset", "fast",
		"-crf", "23",
		"-c:a", "aac",
		"-b:a", "128k",
		"-movflags", "+faststart",
		outputPath,
	)
	burnCmd.Stdout = os.Stdout
	burnCmd.Stderr = os.Stderr
	if err := burnCmd.Run(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Ghép phụ đề thất bại: " + err.Error()})
		return
	}

	// Bước 5: Trả về URL video kết quả
	// Video nằm trong ./tmp/{taskID}/output.mp4, được serve qua route /tmp
	videoURL := fmt.Sprintf("/tmp/%s/output.mp4", taskID)

	log.Printf("[%s] Hoàn tất! Video: %s", taskID, videoURL)
	c.JSON(http.StatusOK, BurnResponse{
		VideoURL: videoURL,
		Message:  "Xử lý thành công!",
	})

	// Dọn dẹp file tạm (video gốc, phụ đề) sau khi trả về, giữ lại output.mp4
	// Có thể bỏ qua để debug
	// os.Remove(videoPath)
	// os.Remove(subPath)
	// os.Remove(assPath)
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

