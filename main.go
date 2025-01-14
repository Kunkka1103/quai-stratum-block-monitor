package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Prometheus 指标文件输出路径
const promFilePath = "/opt/node-exporter/prom/go-quai-stratum-block-number.prom"

// 每次检测的间隔（1 分钟）
const checkInterval = time.Minute

// 正则匹配类似：
// 2025/01/14 10:51:06 Broadcasting block 1513096 to 496 stratum miners
var logRegex = regexp.MustCompile(`^(\d{4}\/\d{2}\/\d{2}\s\d{2}:\d{2}:\d{2})\s+Broadcasting block (\d+) to (\d+) stratum miners`)

// 供主循环判断是否更新
var lastBlockNumber int

func main() {
	// 初始化
	lastBlockNumber = -1

	// 用来实时接收日志中解析到的 blockNumber
	blocksChan := make(chan int, 1000) // 带缓冲，避免堵塞

	// 启动一个 goroutine 实时“流式”读取日志
	go func() {
		err := readBlocksStream("go-quai-stratum", blocksChan)
		if err != nil {
			fmt.Printf("[ERROR] readBlocksStream: %v\n", err)
			// 如果这里出错退出，主程序就收不到日志了
			// 根据需要决定是否重试，或者干脆主程序退出让 Supervisor 重启
		}
	}()

	// 进入主循环，每分钟分析一次
	for {
		startTime := time.Now()

		// 从 channel 中取出“过去这一分钟”里收到的所有区块高度
		blocks := drainBlocksChannel(blocksChan)

		// 执行连续性检查
		isContinuous := checkContinuityIgnoreDuplicates(blocks)

		// 检查是否有更新
		currentBlockNumber := getLatestBlock(blocks)
		isUpdated := 1
		if currentBlockNumber == lastBlockNumber {
			isUpdated = 0
		} else if currentBlockNumber > 0 {
			lastBlockNumber = currentBlockNumber
		}

		// 写入 Prometheus 文件
		err := writePromMetrics(isContinuous, isUpdated)
		if err != nil {
			fmt.Printf("[ERROR] writePromMetrics: %v\n", err)
		}

		// 打印调试日志
		fmt.Printf("[DEBUG] %s => foundBlocks=%d, blocks=%v, continuity=%d, updated=%d, lastBlockNumber=%d\n",
			time.Now().Format("2006-01-02 15:04:05"),
			len(blocks),
			blocks,
			isContinuous,
			isUpdated,
			lastBlockNumber,
		)

		// 等待下一轮
		time.Sleep(checkInterval - time.Since(startTime))
	}
}

// readBlocksStream 用 supervisorctl tail -f 命令实时读取日志行，
// 解析出 blockNumber，送到 blocksChan
func readBlocksStream(serviceName string, blocksChan chan<- int) error {
	// 根据 Supervisor 版本，若要读取 stdout，可能需要加上 "stdout" 参数:
	// supervisorctl tail -f <serviceName> stdout
	// 如果你的 Supervisor 不区分 stdout/stderr，就可以直接 tail -f <serviceName>
	cmd := exec.Command("supervisorctl", "tail", "-f", serviceName, "stdout")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("StdoutPipe error: %w", err)
	}

	// 启动命令
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cmd.Start error: %w", err)
	}

	// 在同一个 goroutine 中做扫描
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		// 可以加些调试输出:
		// fmt.Printf("[TRACE] raw log line: %s\n", line)

		// 解析“Broadcasting block ...”
		if matches := logRegex.FindStringSubmatch(line); len(matches) == 4 {
			blockStr := matches[2] // group(2) 是 block number
			blockNum, err := strconv.Atoi(blockStr)
			if err == nil {
				blocksChan <- blockNum
			}
		}
	}

	// 扫描结束（可能是 supervisorctl 退出）
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanner error: %w", err)
	}

	// 等待子进程退出
	if err := cmd.Wait(); err != nil {
		// supervisorctl tail -f 常常不会正常退出，除非进程被杀
		// 这里记录一下
		return fmt.Errorf("cmd.Wait error: %w", err)
	}

	return nil
}

// drainBlocksChannel 将 channel 中当前可读的数据全部读取出来
// 如果这 1 分钟内日志很多，可能拿到很多 blockNum。否则就少
func drainBlocksChannel(blocksChan chan int) []int {
	var blocks []int
	// 循环取，直到 channel 读不到数据
	for {
		select {
		case b := <-blocksChan:
			blocks = append(blocks, b)
		default:
			// channel 为空时跳出
			return blocks
		}
	}
}

// checkContinuity 判断 blocks 是否“连续”：相邻 block 相差 1
// 这里按顺序检查，如果 blocks 数组是 [100,101,102] 就是连续

// checkContinuityIgnoreDuplicates 忽略重复值，只要块号整体不跳就算连续
func checkContinuityIgnoreDuplicates(blocks []int) int {
	// 若没数据或只有 1 条，默认连续
	if len(blocks) <= 1 {
		return 1
	}

	// 1) 先对 blocks 排序，或者说去重。最简单方式是放到一个 map 再取 key 出来
	unique := make(map[int]bool, len(blocks))
	for _, b := range blocks {
		unique[b] = true
	}

	// 2) 把 key 拿出来做一个有序列表
	distinctBlocks := make([]int, 0, len(unique))
	for k := range unique {
		distinctBlocks = append(distinctBlocks, k)
	}
	// 排序
	// sort.Ints(distinctBlocks)

	// 或者你也可以不排序，而是用“最大块号 - 最小块号 == len-1”来判断是否连续（见下方注释）
	// 这里先演示显式排序 + 遍历

	// sort 出来后，再检查相邻是否相差 1
	sort.Ints(distinctBlocks)
	for i := 1; i < len(distinctBlocks); i++ {
		if distinctBlocks[i] != distinctBlocks[i-1]+1 {
			return 0
		}
	}
	return 1
}

// getLatestBlock 返回最后一个块，如果没有则返回 -1
func getLatestBlock(blocks []int) int {
	if len(blocks) == 0 {
		return -1
	}
	return blocks[len(blocks)-1]
}

// writePromMetrics 将两个指标写入 Prometheus 文件
func writePromMetrics(isContinuous, isUpdated int) error {
	content := strings.Join([]string{
		fmt.Sprintf("quai_stratum_block_number_continuity %d", isContinuous),
		fmt.Sprintf("quai_stratum_block_number_update %d", isUpdated),
		"",
	}, "\n")

	// 确保 /opt/node-exporter/prom 目录存在，且 Node Exporter 能访问
	return os.WriteFile(promFilePath, []byte(content), 0644)
}
