// cfdata 是一个独立的预筛选工具：从 Cloudflare 官方 IP 段中筛出延迟低、
// 位于目标机房的优质 IP。每次运行只测一条链路——由 -baidu-proxy 决定
// 是"直连"还是"经百度前置代理"——产出可以直接喂给转发程序的候选文件。
//
// 想要两条链路各一套结果，跑两次即可：
//
//	cfdata -baidu-proxy=false   得到 cfdata-direct-ip.txt / cfdata-direct-cidr.txt
//	cfdata -baidu-proxy=true    得到 cfdata-baidu-ip.txt  / cfdata-baidu-cidr.txt
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	scanTimeout = 1 * time.Second // 建连/测速超时
	respTimeout = 2 * time.Second // 等待 HTTP 响应超时

	baiduFakeHost  = "sptest.baidu.com"
	baiduUserAgent = "okhttp/3.11.0 Dalvik/2.1.0 (Linux; Build/RKQ1.200826.002) baiduboxapp/11.0.5.12 (Baidu; P1 11)"
	baiduAuthToken = "482857715"

	locationsURL = "https://www.baipiao.eu.org/cloudflare/locations"
	ipv4URL      = "https://www.baipiao.eu.org/cloudflare/ips-v4"
	ipv6URL      = "https://www.baipiao.eu.org/cloudflare/ips-v6"

	locationsFile = "locations.json"
	ipv4File      = "ips-v4.txt"
	ipv6File      = "ips-v6.txt"
)

var (
	randomMu        sync.Mutex
	randomGenerator = rand.New(rand.NewSource(time.Now().UnixNano()))
)

// ── 数据结构 ────────────────────────────────────────────────────────────────

type result struct {
	ip          string
	dataCenter  string
	region      string
	city        string
	latency     string
	tcpDuration time.Duration
}

type location struct {
	Iata   string  `json:"iata"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Cca2   string  `json:"cca2"`
	Region string  `json:"region"`
	City   string  `json:"city"`
}

// ── 百度前置代理池（测速用的 CONNECT 隧道，与转发程序里的逻辑保持一致）───────

type proxyEndpoint struct {
	addr      string
	active    int32
	ewmaNanos int64
	failures  int32
}

type BaiduProxyPool struct {
	name      string
	endpoints []*proxyEndpoint
}

func NewBaiduProxyPool(name string, addrs []string) *BaiduProxyPool {
	pool := &BaiduProxyPool{name: name}
	for _, addr := range dedupeStrings(addrs) {
		pool.endpoints = append(pool.endpoints, &proxyEndpoint{
			addr:      addr,
			ewmaNanos: int64(scanTimeout),
		})
	}
	return pool
}

func (p *BaiduProxyPool) Addresses() []string {
	if p == nil {
		return nil
	}
	addrs := make([]string, 0, len(p.endpoints))
	for _, ep := range p.endpoints {
		addrs = append(addrs, ep.addr)
	}
	return addrs
}

func (p *BaiduProxyPool) pick() *proxyEndpoint {
	if p == nil || len(p.endpoints) == 0 {
		return nil
	}
	if len(p.endpoints) == 1 {
		return p.endpoints[0]
	}
	a := p.endpoints[nextRandomIntn(len(p.endpoints))]
	b := p.endpoints[nextRandomIntn(len(p.endpoints))]
	for b == a && len(p.endpoints) > 1 {
		b = p.endpoints[nextRandomIntn(len(p.endpoints))]
	}
	if b.scoreNanos() < a.scoreNanos() {
		return b
	}
	return a
}

func (p *BaiduProxyPool) Dial(ctx context.Context, targetAddr string, dialTimeout time.Duration) (net.Conn, error) {
	if p == nil || len(p.endpoints) == 0 {
		return nil, fmt.Errorf("百度代理池为空")
	}
	attempts := len(p.endpoints)
	if attempts > 3 {
		attempts = 3
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		ep := p.pick()
		if ep == nil {
			return nil, fmt.Errorf("百度代理池没有可用节点")
		}
		atomic.AddInt32(&ep.active, 1)
		start := time.Now()
		conn, err := dialBaiduTunnelViaNode(ctx, ep.addr, targetAddr, dialTimeout)
		elapsed := time.Since(start)
		if err != nil {
			atomic.AddInt32(&ep.active, -1)
			ep.recordFailure(elapsed)
			lastErr = fmt.Errorf("%s: %w", ep.addr, err)
			continue
		}
		ep.recordSuccess(elapsed)
		return &trackedProxyConn{Conn: conn, endpoint: ep}, nil
	}
	return nil, lastErr
}

func (e *proxyEndpoint) scoreNanos() int64 {
	ewma := atomic.LoadInt64(&e.ewmaNanos)
	if ewma <= 0 {
		ewma = int64(scanTimeout)
	}
	active := int64(atomic.LoadInt32(&e.active))
	failures := int64(atomic.LoadInt32(&e.failures))
	return ewma + active*int64(50*time.Millisecond) + failures*int64(300*time.Millisecond)
}

func (e *proxyEndpoint) recordSuccess(elapsed time.Duration) {
	updateEWMA(&e.ewmaNanos, elapsed)
	if atomic.LoadInt32(&e.failures) > 0 {
		atomic.AddInt32(&e.failures, -1)
	}
}

func (e *proxyEndpoint) recordFailure(elapsed time.Duration) {
	if elapsed > 0 {
		updateEWMA(&e.ewmaNanos, elapsed)
	}
	atomic.AddInt32(&e.failures, 1)
}

type trackedProxyConn struct {
	net.Conn
	endpoint *proxyEndpoint
	once     sync.Once
}

func (c *trackedProxyConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		atomic.AddInt32(&c.endpoint.active, -1)
	})
	return err
}

func updateEWMA(dst *int64, sample time.Duration) {
	if sample <= 0 {
		return
	}
	sampleNanos := int64(sample)
	for {
		old := atomic.LoadInt64(dst)
		next := sampleNanos
		if old > 0 {
			next = (old*7 + sampleNanos) / 8
		}
		if atomic.CompareAndSwapInt64(dst, old, next) {
			return
		}
	}
}

// ── 主入口 ───────────────────────────────────────────────────────────────────

func main() {
	coloFilter := flag.String("colo", "", "筛选数据中心，例如 HKG,SJC（留空忽略）")
	ipCount := flag.Int("ipnum", 0, "提取的有效IP数量，0表示不限制")
	ipsType := flag.String("ips", "4", "IPv4或IPv6 (4或6)")
	maxThreads := flag.Int("task", 100, "并发协程数")
	random := flag.Bool("random", true, "是否随机生成IP，如果为false，则从CIDR中拆分出所有IP")
	useBaiduProxy := flag.Bool("baidu-proxy", true, "测速链路：true=经百度前置代理隧道测速，false=直连测速")
	baiduDomain := flag.String("baidu-domain", "cloudnproxy.baidu.com", "百度前置代理域名")
	baiduPort := flag.Int("baidu-port", 443, "百度前置代理端口")
	output := flag.String("output", "cfdata", "输出文件前缀，自动生成 {output}-{direct|baidu}-ip.txt 与 {output}-{direct|baidu}-cidr.txt")
	flag.Parse()

	label := "direct"
	var proxyPool *BaiduProxyPool
	if *useBaiduProxy {
		label = "baidu"
		proxyPool = NewBaiduProxyPool("default", []string{ensureHostPort(*baiduDomain, *baiduPort)})
		log.Printf("测速链路: 经百度前置代理 %s", strings.Join(proxyPool.Addresses(), ","))
	} else {
		log.Printf("测速链路: 直连")
	}

	// 1. 加载机房位置信息
	locations, err := loadLocations()
	if err != nil {
		log.Fatalf("加载位置信息失败: %v", err)
	}
	locationMap := make(map[string]location)
	for _, loc := range locations {
		locationMap[loc.Iata] = loc
	}

	// 2. 加载候选 IP
	ipList, err := loadCandidateIPs(*ipsType, *random)
	if err != nil {
		log.Fatalf("加载候选IP失败: %v", err)
	}
	log.Printf("候选IP数量: %d", len(ipList))

	// 3. 扫描
	startTime := time.Now()
	results := scanIPs(ipList, locationMap, *maxThreads, proxyPool)
	if len(results) == 0 {
		log.Fatal("未发现任何有效IP")
	}

	// 4. 数据中心过滤
	if *coloFilter != "" {
		filters := strings.Split(*coloFilter, ",")
		var filtered []result
		for _, r := range results {
			for _, f := range filters {
				if strings.EqualFold(r.dataCenter, strings.TrimSpace(f)) {
					filtered = append(filtered, r)
					break
				}
			}
		}
		results = filtered
	}
	if len(results) == 0 {
		log.Fatal("按 -colo 过滤后没有剩余IP")
	}

	// 5. 按延迟排序，截取
	sort.Slice(results, func(i, j int) bool {
		return results[i].tcpDuration < results[j].tcpDuration
	})
	if *ipCount > 0 && len(results) > *ipCount {
		results = results[:*ipCount]
	}

	// 6. 打印结果
	fmt.Printf("\n[%s] IP 地址 | 数据中心 | 地区 | 城市 | 延迟\n", label)
	for _, r := range results {
		fmt.Printf("%s | %s | %s | %s | %s\n", r.ip, r.dataCenter, r.region, r.city, r.latency)
	}
	fmt.Printf("\n[%s] 成功提取 %d 个优质IP，耗时 %s\n", label, len(results), time.Since(startTime).Round(time.Second))

	// 7. 落盘：逐个精确IP + 去重后的网段
	writeResultFiles(results, *ipsType, *output, label)
}

// writeResultFiles 落盘两份文件：逐个精确IP、以及去重后的 /24（v4）或 /48（v6）网段。
func writeResultFiles(results []result, ipsType, outPrefix, label string) {
	ipFile := fmt.Sprintf("%s-%s-ip.txt", outPrefix, label)
	cidrFile := fmt.Sprintf("%s-%s-cidr.txt", outPrefix, label)

	ips := make([]string, 0, len(results))
	cidrSet := make(map[string]struct{})
	var cidrList []string
	for _, r := range results {
		ips = append(ips, r.ip)
		cidr := toCIDR(r.ip, ipsType)
		if cidr == "" {
			continue
		}
		if _, ok := cidrSet[cidr]; !ok {
			cidrSet[cidr] = struct{}{}
			cidrList = append(cidrList, cidr)
		}
	}

	if err := writeLines(ipFile, ips); err != nil {
		log.Fatalf("写入 %s 失败: %v", ipFile, err)
	}
	log.Printf("已写入 %d 个精确IP 到 %s", len(ips), ipFile)

	if err := writeLines(cidrFile, cidrList); err != nil {
		log.Fatalf("写入 %s 失败: %v", cidrFile, err)
	}
	log.Printf("已写入 %d 个网段 到 %s", len(cidrList), cidrFile)
}

func writeLines(filename string, lines []string) error {
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return w.Flush()
}

// toCIDR 把发现的优质 IP 归并成一个"周边网段"：IPv4 归并到 /24，IPv6 归并到 /48。
// 这与候选源文件本身的前缀长度无关，只是为转发程序提供一个有一定容错空间的
// 备选范围（原地址失效时还能在同一小网段内再试其他地址）。
func toCIDR(ip string, ipsType string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	if ipsType == "6" {
		v6 := parsed.To16()
		if v6 == nil {
			return ""
		}
		masked := make(net.IP, 16)
		copy(masked, v6)
		for i := 6; i < 16; i++ {
			masked[i] = 0
		}
		return masked.String() + "/48"
	}
	v4 := parsed.To4()
	if v4 == nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.0/24", v4[0], v4[1], v4[2])
}

// ── 候选IP / 机房数据加载 ────────────────────────────────────────────────────

func loadCandidateIPs(ipsType string, random bool) ([]string, error) {
	var url, filename string
	switch ipsType {
	case "6":
		filename = ipv6File
		url = ipv6URL
	case "4":
		filename = ipv4File
		url = ipv4URL
	default:
		return nil, fmt.Errorf("无效的IP类型，请使用 '4' 或 '6'")
	}

	var content string
	var err error
	if _, err = os.Stat(filename); os.IsNotExist(err) {
		fmt.Printf("文件 %s 不存在，正在从 %s 下载\n", filename, url)
		content, err = getURLContent(url)
		if err != nil {
			return nil, fmt.Errorf("获取URL内容出错: %w", err)
		}
		if err = saveToFile(filename, content); err != nil {
			return nil, fmt.Errorf("保存文件出错: %w", err)
		}
	} else {
		content, err = getFileContent(filename)
		if err != nil {
			return nil, fmt.Errorf("读取本地文件出错: %w", err)
		}
	}

	if random {
		return pickRandomIPs(parseIPList(content)), nil
	}
	return readIPs(filename)
}

func loadLocations() ([]location, error) {
	var locations []location

	if _, err := os.Stat(locationsFile); os.IsNotExist(err) {
		fmt.Printf("%s 不存在，正在从 %s 下载\n", locationsFile, locationsURL)
		resp, err := http.Get(locationsURL)
		if err != nil {
			return nil, fmt.Errorf("无法获取JSON: %v", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("无法读取响应体: %v", err)
		}
		if err = json.Unmarshal(body, &locations); err != nil {
			return nil, fmt.Errorf("无法解析JSON: %v", err)
		}
		if err = saveToFile(locationsFile, string(body)); err != nil {
			return nil, fmt.Errorf("无法写入文件: %v", err)
		}
	} else {
		body, err := getFileContent(locationsFile)
		if err != nil {
			return nil, fmt.Errorf("无法读取文件: %v", err)
		}
		if err = json.Unmarshal([]byte(body), &locations); err != nil {
			return nil, fmt.Errorf("无法解析JSON: %v", err)
		}
	}
	return locations, nil
}

// ── 扫描逻辑 ─────────────────────────────────────────────────────────────────

func scanIPs(ipList []string, locationMap map[string]location, maxThreads int, proxyPool *BaiduProxyPool) []result {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var results []result

	thread := make(chan struct{}, maxThreads)
	var count int32
	total := len(ipList)

	for _, ip := range ipList {
		wg.Add(1)
		thread <- struct{}{}
		go func(ipAddr string) {
			defer func() {
				<-thread
				wg.Done()
				current := atomic.AddInt32(&count, 1)
				percentage := float64(current) / float64(total) * 100
				fmt.Printf("已完成: %d 总数: %d 已完成: %.2f%%\r", current, total, percentage)
				if int(current) == total {
					fmt.Printf("已完成: %d 总数: %d 已完成: %.2f%%\n", current, total, percentage)
				}
			}()

			start := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
			defer cancel()
			conn, err := dialTarget(ctx, "tcp", net.JoinHostPort(ipAddr, "80"), scanTimeout, proxyPool)
			if err != nil {
				return
			}
			defer conn.Close()

			tcpDuration := time.Since(start)

			requestURL := "http://" + net.JoinHostPort(ipAddr, "80")
			req, err := http.NewRequest("GET", requestURL, nil)
			if err != nil {
				return
			}
			req.Header.Set("User-Agent", "Mozilla/5.0")
			req.Close = true

			conn.SetDeadline(time.Now().Add(respTimeout))
			if err = req.Write(conn); err != nil {
				return
			}

			reader := bufio.NewReader(conn)
			resp, err := http.ReadResponse(reader, req)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			cfRay := strings.TrimSpace(resp.Header.Get("CF-RAY"))
			if cfRay == "" {
				return
			}
			parts := strings.Split(cfRay, "-")
			if len(parts) < 2 {
				return
			}
			dataCenter := strings.TrimSpace(parts[len(parts)-1])
			if dataCenter == "" {
				return
			}

			loc, ok := locationMap[dataCenter]
			mu.Lock()
			if ok {
				fmt.Printf("发现有效IP %s 位置: %s 延迟: %d ms\n", ipAddr, loc.City, tcpDuration.Milliseconds())
				results = append(results, result{ipAddr, dataCenter, loc.Region, loc.City, fmt.Sprintf("%d ms", tcpDuration.Milliseconds()), tcpDuration})
			} else {
				fmt.Printf("发现有效IP %s 位置未知 延迟: %d ms\n", ipAddr, tcpDuration.Milliseconds())
				results = append(results, result{ipAddr, dataCenter, "", "", fmt.Sprintf("%d ms", tcpDuration.Milliseconds()), tcpDuration})
			}
			mu.Unlock()
		}(ip)
	}

	wg.Wait()
	return results
}

func dialTarget(ctx context.Context, network, targetAddr string, dialTimeout time.Duration, proxyPool *BaiduProxyPool) (net.Conn, error) {
	if proxyPool != nil {
		return proxyPool.Dial(ctx, targetAddr, dialTimeout)
	}
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 0}
	return dialer.DialContext(ctx, network, targetAddr)
}

func dialBaiduTunnelViaNode(ctx context.Context, nodeAddr, targetAddr string, dialTimeout time.Duration) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 0}
	conn, err := dialer.DialContext(ctx, "tcp", nodeAddr)
	if err != nil {
		return nil, fmt.Errorf("连接百度前置代理失败: %w", err)
	}

	deadline := time.Now().Add(dialTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, fmt.Errorf("设置超时失败: %w", err)
	}

	connectReq := fmt.Sprintf(
		"CONNECT %s HTTP/1.1\r\n"+
			"Host: %s\r\n"+
			"X-T5-Auth: %s\r\n"+
			"User-Agent: %s\r\n"+
			"Proxy-Connection: keep-alive\r\n"+
			"Connection: keep-alive\r\n"+
			"\r\n",
		targetAddr, baiduFakeHost, baiduAuthToken, baiduUserAgent,
	)
	if _, err := conn.Write([]byte(connectReq)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("写入CONNECT失败: %w", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("读取CONNECT响应失败: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("百度代理CONNECT被拒绝: %s", resp.Status)
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("清除超时失败: %w", err)
	}
	return conn, nil
}

// ── 辅助函数 ─────────────────────────────────────────────────────────────────

func getURLContent(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP请求失败，状态码: %d", resp.StatusCode)
	}
	var content strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			content.WriteString(line + "\n")
		}
	}
	return content.String(), scanner.Err()
}

func getFileContent(filename string) (string, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func saveToFile(filename, content string) error {
	return os.WriteFile(filename, []byte(content), 0644)
}

func parseIPList(content string) []string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	var list []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			list = append(list, line)
		}
	}
	return list
}

func nextRandomIntn(n int) int {
	randomMu.Lock()
	defer randomMu.Unlock()
	return randomGenerator.Intn(n)
}

// pickRandomIPs 从每条 CIDR 网段中随机选取一个属于该网段的地址。
// 用 net.ParseCIDR 按网段实际的前缀长度随机化主机位，v4/v6 共用同一套逻辑，
// 对当前固定为 /24（v4）、/48（v6）的数据源等价于"只随机最后一段/最后几组"，
// 但不用为两种地址族各写一份重复代码。
func pickRandomIPs(cidrList []string) []string {
	out := make([]string, 0, len(cidrList))
	for _, line := range cidrList {
		ip, err := randomIPInCIDR(line)
		if err != nil {
			log.Printf("忽略无法解析的网段 %q: %v", line, err)
			continue
		}
		out = append(out, ip)
	}
	return out
}

func randomIPInCIDR(cidr string) (string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", err
	}
	ones, bits := ipnet.Mask.Size()
	hostBits := bits - ones

	var addr net.IP
	if bits == 32 {
		addr = make(net.IP, 4)
		copy(addr, ipnet.IP.To4())
	} else {
		addr = make(net.IP, 16)
		copy(addr, ipnet.IP.To16())
	}

	remaining := hostBits
	for i := len(addr) - 1; i >= 0 && remaining > 0; i-- {
		if remaining >= 8 {
			addr[i] = byte(nextRandomIntn(256))
			remaining -= 8
		} else {
			mask := byte(1<<uint(remaining)) - 1
			addr[i] = (addr[i] &^ mask) | byte(nextRandomIntn(int(mask)+1))
			remaining = 0
		}
	}
	return addr.String(), nil
}

// 从CIDR中拆分出所有IP
func readIPs(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var ips []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.Contains(line, "/") {
			ipAddr, ipNet, err := net.ParseCIDR(line)
			if err != nil {
				return nil, err
			}
			for cur := ipAddr.Mask(ipNet.Mask); ipNet.Contains(cur); incrementIP(cur) {
				ips = append(ips, cur.String())
			}
		} else {
			ips = append(ips, line)
		}
	}
	return ips, scanner.Err()
}

func incrementIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

func ensureHostPort(addr string, port int) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return addr
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(strings.Trim(addr, "[]"), strconv.Itoa(port))
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	var out []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
