package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Prometheus 指标文件输出路径
const promFilePath = "/opt/node-exporter/prom/go-quai-stratum.prom"

// 每次检测的间隔
const checkInterval = time.Minute

// supervisorctl tail --lines=XXX 读取多少行日志，用于截取“最近”日志
// 具体行数可以根据你的日志产生速率适度调整
const tailLines = "300"

// 正则示例：
//  2025/01/14 09:55:34 Broadcasting block 1512464 to 405 stratum miners
//  ^(2025/01/14 09:55:34) Broadcasting block (1512464) to (405) stratum miners
// 时间格式：2006/01/02 15:04:05
var logRegex = regexp.MustCompile(
	`^(\d{4}\/\d{2}\/\d{2}\s\d{2}:\d{2}:\d{2})\s+Broadcasting block (\d+) to (\d+) stratum miners`,
)

// 维护一个上次记录的最高区块，用于判断是否“更新”
var lastBlockNumber int

func main() {
	// 第一次跑可以把 lastBlockNumber 设置成 -1 或 0，表示还没拿过区块
	lastBlockNumber = -1

	for {
		// 每次循环的起始时间
		startTime := time.Now()

		// 读取“过去一分钟”内的所有区块高度
		blocks, err := readRecentBlocks("go-quai-stratum", startTime.Add(-checkInterval))
		if err != nil {
			fmt.Printf("[ERROR] readRecentBlocks: %v\n", err)
			// 即使报错，也要等下一个周期重试
			sleepUntilNext(startTime)
			continue
		}

		// 连续性检查
		isContinuous := checkContinuity(blocks)

		// 是否更新检查
		currentBlockNumber := getLatestBlock(blocks)
		isUpdated := 1
		if currentBlockNumber == lastBlockNumber {
			isUpdated = 0
		} else if currentBlockNumber > 0 {
			// 当拿到有效的最新区块时才更新全局记录
			lastBlockNumber = currentBlockNumber
		}

		// 写入 Prom 文件
		if err := writePromMetrics(isContinuous, isUpdated); err != nil {
			fmt.Printf("[ERROR] writePromMetrics: %v\n", err)
		}

		// 打印一些调试日志，便于观察
		fmt.Printf(
			"[DEBUG] time=%s, foundBlocks=%d, blocks=%v, continuity=%d, updated=%d, lastBlockNumber=%d\n",
			time.Now().Format("2006-01-02 15:04:05"),
			len(blocks),
			blocks,
			isContinuous,
			isUpdated,
			lastBlockNumber,
		)

		// 等待下一次循环
		sleepUntilNext(startTime)
	}
}

// readRecentBlocks 调用 `supervisorctl tail <serviceName> --lines=300` 读取最近若干行日志，
// 并从中筛选出在 afterTime 之后的日志行，返回解析得到的 block 列表。
func readRecentBlocks(serviceName string, afterTime time.Time) ([]int, error) {
	// 调 supervisorctl:  tail --lines=300 <serviceName>
	cmd := exec.Command("supervisorctl", "tail", serviceName, "--lines="+tailLines)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("cmd.StdoutPipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cmd.Start: %w", err)
	}

	lines := make([]string, 0)
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanner.Err: %w", err)
	}

	// 等待子进程退出
	if err := cmd.Wait(); err != nil {
		// supervisorctl tail 命令可能返回状态码非0，不一定就是错误
		// 这里简单记录一下
		fmt.Printf("[WARN] supervisorctl tail exit: %v\n", err)
	}

	fmt.Printf("[DEBUG] readRecentBlocks: total lines read=%d\n", len(lines))

	// 从这些行里，提取在 afterTime 之后的 block
	blocks := make([]int, 0)
	for _, line := range lines {
		t, blockNum, ok := parseTimeAndBlock(line)
		if !ok {
			// 不符合正则或解析失败就跳过
			continue
		}
		if t.After(afterTime) {
			blocks = append(blocks, blockNum)
		}
	}

	fmt.Printf("[DEBUG] readRecentBlocks: lines after %s => blocks=%v\n",
		afterTime.Format("15:04:05"),
		blocks,
	)

	return blocks, nil
}

// parseTimeAndBlock 从一行形如
//   "2025/01/14 09:55:34 Broadcasting block 1512464 to 405 stratum miners"
// 中解析出 (time, blockNumber)。若解析成功返回 (t, blockNum, true)，否则 (零值, 0, false)。
func parseTimeAndBlock(line string) (time.Time, int, bool) {
	matches := logRegex.FindStringSubmatch(line)
	if len(matches) != 4 {
		return time.Time{}, 0, false
	}
	// 解析时间
	tStr := matches[1] // "2025/01/14 09:55:34"
	blockStr := matches[2] // "1512464"
	// minersStr := matches[3] // "405" (如果需要，可以留着)

	t, err := time.Parse("2006/01/02 15:04:05", tStr)
	if err != nil {
		return time.Time{}, 0, false
	}
	blockNum, err := strconv.Atoi(blockStr)
	if err != nil {
		return time.Time{}, 0, false
	}

	return t, blockNum, true
}

// checkContinuity 判断 blocks 是否严格连续
// 如果只有 0 或 1 个区块，也可以视为连续（返回1）
func checkContinuity(blocks []int) int {
	if len(blocks) <= 1 {
		return 1
	}
	for i := 1; i < len(blocks); i++ {
		if blocks[i] != blocks[i-1]+1 {
			return 0
		}
	}
	return 1
}

// getLatestBlock 返回 blocks 中的最大值，若 blocks 为空则返回 -1
func getLatestBlock(blocks []int) int {
	if len(blocks) == 0 {
		return -1
	}
	return blocks[len(blocks)-1]
}

// writePromMetrics 将两个指标写入 promFilePath
func writePromMetrics(isContinuous, isUpdated int) error {
	content := strings.Join([]string{
		fmt.Sprintf("quai_stratum_block_number_continuity %d", isContinuous),
		fmt.Sprintf("quai_stratum_block_number_update %d", isUpdated),
		"",
	}, "\n")

	// 若 /opt/node-exporter/prom 目录不存在，需要手动创建并赋予权限
	return os.WriteFile(promFilePath, []byte(content), 0644)
}

// sleepUntilNext 让程序休眠到下个 checkInterval 周期
func sleepUntilNext(start time.Time) {
	sleepDuration := checkInterval - time.Since(start)
	if sleepDuration > 0 {
		time.Sleep(sleepDuration)
	}
}
