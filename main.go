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

const (
	promFilePath  = "/opt/node-exporter/prom/go-quai-stratum.prom"
	checkInterval = time.Minute // 检测间隔
)

var (
	blockRegex = regexp.MustCompile(`Broadcasting block (\d+) to \d+ stratum miners`)
)

func main() {
	var lastBlockNumber int
	// var lastUpdateTime time.Time // 如果未来需要扩展，可以保留这一行

	for {
		startTime := time.Now()

		// 使用 supervisorctl tail -f 获取日志
		blocks, err := readBlocksFromSupervisor("go-quai-stratum")
		if err != nil {
			fmt.Printf("Error reading log from supervisorctl: %v\n", err)
			time.Sleep(checkInterval)
			continue
		}

		// 检查高度连续性
		isContinuous := checkContinuity(blocks)

		// 检查高度更新
		currentBlockNumber := getLatestBlock(blocks)
		isUpdated := 1
		if currentBlockNumber == lastBlockNumber {
			isUpdated = 0
		} else {
			lastBlockNumber = currentBlockNumber
			// lastUpdateTime = startTime // 如果需要记录更新时间，可以启用
		}

		// 写入 Prometheus 指标
		err = writePromMetrics(isContinuous, isUpdated)
		if err != nil {
			fmt.Printf("Error writing metrics: %v\n", err)
		}

		// 等待下一次检查
		sleepDuration := checkInterval - time.Since(startTime)
		if sleepDuration > 0 {
			time.Sleep(sleepDuration)
		}
	}
}

// 使用 supervisorctl tail -f 获取日志
func readBlocksFromSupervisor(serviceName string) ([]int, error) {
	cmd := exec.Command("supervisorctl", "tail", "-f", serviceName)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	defer cmd.Process.Kill() // 确保子进程结束
	defer cmd.Wait()

	var blocks []int
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if matches := blockRegex.FindStringSubmatch(line); len(matches) > 1 {
			blockNumber, _ := strconv.Atoi(matches[1])
			blocks = append(blocks, blockNumber)
		}
	}
	return blocks, scanner.Err()
}

// 检查高度是否连续
func checkContinuity(blocks []int) int {
	if len(blocks) <= 1 {
		return 1 // 少于两个区块，视为连续
	}

	for i := 1; i < len(blocks); i++ {
		if blocks[i] != blocks[i-1]+1 {
			return 0 // 发现不连续
		}
	}
	return 1 // 全部连续
}

// 获取最新的区块高度
func getLatestBlock(blocks []int) int {
	if len(blocks) == 0 {
		return -1 // 没有区块
	}
	return blocks[len(blocks)-1]
}

// 写入 Prometheus 指标
func writePromMetrics(isContinuous, isUpdated int) error {
	metrics := []string{
		fmt.Sprintf(`quai_stratum_block_number_continuity %d`, isContinuous),
		fmt.Sprintf(`quai_stratum_block_number_update %d`, isUpdated),
	}

	content := strings.Join(metrics, "\n") + "\n"
	return os.WriteFile(promFilePath, []byte(content), 0644)
}
