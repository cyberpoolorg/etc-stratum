package payouts

import (
	"fmt"
	"log"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common/math"

	"github.com/cyberpoolorg/etc-stratum/rpc"
	"github.com/cyberpoolorg/etc-stratum/storage"
	"github.com/cyberpoolorg/etc-stratum/util"
)

type UnlockerConfig struct {
	Enabled           bool     `json:"enabled"`
	PoolFee           float64  `json:"poolFee"`
	PoolFeeAddress    string   `json:"poolFeeAddress"`
	Donate            bool     `json:"donate"`
	Depth             int64    `json:"depth"`
	ImmatureDepth     int64    `json:"immatureDepth"`
	KeepTxFees        bool     `json:"keepTxFees"`
	Interval          string   `json:"interval"`
	Daemon            string   `json:"daemon"`
	Timeout           string   `json:"timeout"`
	Ecip1017FBlock    int64    `json:"ecip1017FBlock"`
	Ecip1017EraRounds *big.Int `json:"ecip1017EraRounds"`
}

const minDepth = 16

var disinflationRateQuotient = big.NewInt(4)
var disinflationRateDivisor = big.NewInt(5)
var big32 = big.NewInt(32)
var big8 = big.NewInt(8)

var homesteadReward = math.MustParseBig256("5000000000000000000")

type BlockUnlocker struct {
	config   *UnlockerConfig
	backend  *storage.RedisClient
	rpc      *rpc.RPCClient
	halt     bool
	lastFail error
}

func NewBlockUnlocker(cfg *UnlockerConfig, backend *storage.RedisClient, network *string) *BlockUnlocker {
	if *network == "classic" {
		cfg.Ecip1017FBlock = 5000000
		cfg.Ecip1017EraRounds = big.NewInt(5000000)
	} else if *network == "mordor" {
		cfg.Ecip1017FBlock = 0
		cfg.Ecip1017EraRounds = big.NewInt(2000000)
	} else {
		log.Fatalln("Invalid network set", network)
	}

	if len(cfg.PoolFeeAddress) != 0 && !util.IsValidHexAddress(cfg.PoolFeeAddress) {
		log.Fatalln("Invalid poolFeeAddress", cfg.PoolFeeAddress)
	}
	if cfg.Depth < minDepth*2 {
		log.Fatalf("Block maturity depth can't be < %v, your depth is %v", minDepth*2, cfg.Depth)
	}
	if cfg.ImmatureDepth < minDepth {
		log.Fatalf("Immature depth can't be < %v, your depth is %v", minDepth, cfg.ImmatureDepth)
	}
	u := &BlockUnlocker{config: cfg, backend: backend}
	var err error
	u.rpc, err = rpc.NewRPCClient("BlockUnlocker", cfg.Daemon, cfg.Timeout)
	if err != nil {
		log.Fatalf("Failed to start RPC client: %v", err)
	}
	return u
}

func (u *BlockUnlocker) Start() {
	log.Println("Starting block unlocker")
	intv := util.MustParseDuration(u.config.Interval)
	timer := time.NewTimer(intv)
	log.Printf("Set block unlock interval to %v", intv)

	u.unlockPendingBlocks()
	u.unlockAndCreditMiners()
	timer.Reset(intv)

	go func() {
		for {
			select {
			case <-timer.C:
				u.unlockPendingBlocks()
				u.unlockAndCreditMiners()
				timer.Reset(intv)
			}
		}
	}()
}

type UnlockResult struct {
	maturedBlocks  []*storage.BlockData
	orphanedBlocks []*storage.BlockData
	orphans        int
	uncles         int
	blocks         int
}

func (u *BlockUnlocker) unlockCandidates(candidates []*storage.BlockData) (*UnlockResult, error) {
	result := &UnlockResult{}

	for _, candidate := range candidates {
		orphan := true

		if candidate.Height < minDepth {
			orphan = false
			// avoid scanning the first 16 blocks
			continue
		}
		for i := int64(minDepth * -1); i < minDepth; i++ {
			height := candidate.Height + i

			if height < 0 {
				continue
			}

			block, err := u.rpc.GetBlockByHeight(height)

			if err != nil {
				log.Printf("Error while retrieving block %v from node: %v", height, err)
				return nil, err
			}
			if block == nil {
				return nil, fmt.Errorf("Error while retrieving block %v from node, wrong node height", height)
			}

			if matchCandidate(block, candidate) {
				orphan = false
				result.blocks++

				err = u.handleBlock(block, candidate)
				if err != nil {
					u.halt = true
					u.lastFail = err
					return nil, err
				}
				result.maturedBlocks = append(result.maturedBlocks, candidate)
				log.Printf("Mature block %v with %v tx, hash: %v", candidate.Height, len(block.Transactions), candidate.Hash[0:10])
				break
			}

			if len(block.Uncles) == 0 {
				continue
			}

			for uncleIndex, uncleHash := range block.Uncles {
				uncle, err := u.rpc.GetUncleByBlockNumberAndIndex(height, uncleIndex)
				if err != nil {
					return nil, fmt.Errorf("Error while retrieving uncle of block %v from node: %v", uncleHash, err)
				}
				if uncle == nil {
					return nil, fmt.Errorf("Error while retrieving uncle of block %v from node", height)
				}

				// Found uncle
				if matchCandidate(uncle, candidate) {
					orphan = false
					result.uncles++

					err := handleUncle(height, uncle, candidate, u.config)
					if err != nil {
						u.halt = true
						u.lastFail = err
						return nil, err
					}
					result.maturedBlocks = append(result.maturedBlocks, candidate)
					log.Printf("Mature uncle %v/%v of reward %v with hash: %v", candidate.Height, candidate.UncleHeight,
						util.FormatReward(candidate.Reward), uncle.Hash[0:10])
					break
				}
			}

			if !orphan {
				break
			}
		}

		if orphan {
			result.orphans++
			candidate.Orphan = true
			result.orphanedBlocks = append(result.orphanedBlocks, candidate)
			log.Printf("Orphaned block %v:%v", candidate.RoundHeight, candidate.Nonce)
		}
	}
	return result, nil
}

func matchCandidate(block *rpc.GetBlockReply, candidate *storage.BlockData) bool {

	if len(candidate.Hash) > 0 && strings.EqualFold(candidate.Hash, block.Hash) {
		return true
	}

	if len(block.Nonce) > 0 {
		return strings.EqualFold(block.Nonce, candidate.Nonce)
	}

	if len(block.SealFields) == 2 {
		return strings.EqualFold(candidate.Nonce, block.SealFields[1])
	}
	return false
}

func (u *BlockUnlocker) handleBlock(block *rpc.GetBlockReply, candidate *storage.BlockData) error {
	correctHeight, err := strconv.ParseInt(strings.Replace(block.Number, "0x", "", -1), 16, 64)
	if err != nil {
		return err
	}
	candidate.Height = correctHeight
	era := GetBlockEra(big.NewInt(candidate.Height), u.config.Ecip1017EraRounds)
	reward := getConstReward(era)

	uncleReward := getRewardForUncle(reward)
	rewardForUncles := big.NewInt(0).Mul(uncleReward, big.NewInt(int64(len(block.Uncles))))
	reward.Add(reward, rewardForUncles)

	extraTxReward, err := u.getExtraRewardForTx(block)
	if err != nil {
		return fmt.Errorf("Error while fetching TX receipt: %v", err)
	}
	if u.config.KeepTxFees {
		candidate.ExtraReward = extraTxReward
	} else {
		reward.Add(reward, extraTxReward)
	}

	candidate.Orphan = false
	candidate.Hash = block.Hash
	candidate.Reward = reward
	return nil
}

func handleUncle(height int64, uncle *rpc.GetBlockReply, candidate *storage.BlockData, cfg *UnlockerConfig) error {
	uncleHeight, err := strconv.ParseInt(strings.Replace(uncle.Number, "0x", "", -1), 16, 64)
	if err != nil {
		return err
	}
	era := GetBlockEra(big.NewInt(height), cfg.Ecip1017EraRounds)
	reward := getUncleReward(new(big.Int).SetInt64(uncleHeight), new(big.Int).SetInt64(height), era, getConstReward(era))
	candidate.Height = height
	candidate.UncleHeight = uncleHeight
	candidate.Orphan = false
	candidate.Hash = uncle.Hash
	candidate.Reward = reward
	return nil
}

func (u *BlockUnlocker) unlockPendingBlocks() {
	if u.halt {
		log.Println("Unlocking suspended due to last critical error:", u.lastFail)
		return
	}

	current, err := u.rpc.GetPendingBlock()
	if err != nil {
		u.halt = true
		u.lastFail = err
		log.Printf("Unable to get current blockchain height from node: %v", err)
		return
	}
	currentHeight, err := strconv.ParseInt(strings.Replace(current.Number, "0x", "", -1), 16, 64)
	if err != nil {
		u.halt = true
		u.lastFail = err
		log.Printf("Can't parse pending block number: %v", err)
		return
	}

	candidates, err := u.backend.GetCandidates(currentHeight - u.config.ImmatureDepth)
	if err != nil {
		u.halt = true
		u.lastFail = err
		log.Printf("Failed to get block candidates from backend: %v", err)
		return
	}

	if len(candidates) == 0 {
		log.Println("No block candidates to unlock")
		return
	}

	result, err := u.unlockCandidates(candidates)
	if err != nil {
		u.halt = true
		u.lastFail = err
		log.Printf("Failed to unlock blocks: %v", err)
		return
	}
	log.Printf("Immature %v blocks, %v uncles, %v orphans", result.blocks, result.uncles, result.orphans)

	err = u.backend.WritePendingOrphans(result.orphanedBlocks)
	if err != nil {
		u.halt = true
		u.lastFail = err
		log.Printf("Failed to insert orphaned blocks into backend: %v", err)
		return
	} else {
		log.Printf("Inserted %v orphaned blocks to backend", result.orphans)
	}

	totalRevenue := new(big.Rat)
	totalMinersProfit := new(big.Rat)
	totalPoolProfit := new(big.Rat)

	for _, block := range result.maturedBlocks {
		revenue, minersProfit, poolProfit, roundRewards, err := u.calculateRewards(block)
		if err != nil {
			u.halt = true
			u.lastFail = err
			log.Printf("Failed to calculate rewards for round %v: %v", block.RoundKey(), err)
			return
		}
		err = u.backend.WriteImmatureBlock(block, roundRewards)
		if err != nil {
			u.halt = true
			u.lastFail = err
			log.Printf("Failed to credit rewards for round %v: %v", block.RoundKey(), err)
			return
		}
		totalRevenue.Add(totalRevenue, revenue)
		totalMinersProfit.Add(totalMinersProfit, minersProfit)
		totalPoolProfit.Add(totalPoolProfit, poolProfit)

		logEntry := fmt.Sprintf(
			"IMMATURE %v: revenue %v, miners profit %v, pool profit: %v",
			block.RoundKey(),
			util.FormatRatReward(revenue),
			util.FormatRatReward(minersProfit),
			util.FormatRatReward(poolProfit),
		)
		entries := []string{logEntry}
		for login, reward := range roundRewards {
			entries = append(entries, fmt.Sprintf("\tREWARD %v: %v: %v Shannon", block.RoundKey(), login, reward))
		}
		log.Println(strings.Join(entries, "\n"))
	}

	log.Printf(
		"IMMATURE SESSION: revenue %v, miners profit %v, pool profit: %v",
		util.FormatRatReward(totalRevenue),
		util.FormatRatReward(totalMinersProfit),
		util.FormatRatReward(totalPoolProfit),
	)
}

func (u *BlockUnlocker) unlockAndCreditMiners() {
	if u.halt {
		log.Println("Unlocking suspended due to last critical error:", u.lastFail)
		return
	}

	current, err := u.rpc.GetPendingBlock()
	if err != nil {
		u.halt = true
		u.lastFail = err
		log.Printf("Unable to get current blockchain height from node: %v", err)
		return
	}
	currentHeight, err := strconv.ParseInt(strings.Replace(current.Number, "0x", "", -1), 16, 64)
	if err != nil {
		u.halt = true
		u.lastFail = err
		log.Printf("Can't parse pending block number: %v", err)
		return
	}

	immature, err := u.backend.GetImmatureBlocks(currentHeight - u.config.Depth)
	if err != nil {
		u.halt = true
		u.lastFail = err
		log.Printf("Failed to get block candidates from backend: %v", err)
		return
	}

	if len(immature) == 0 {
		log.Println("No immature blocks to credit miners")
		return
	}

	result, err := u.unlockCandidates(immature)
	if err != nil {
		u.halt = true
		u.lastFail = err
		log.Printf("Failed to unlock blocks: %v", err)
		return
	}
	log.Printf("Unlocked %v blocks, %v uncles, %v orphans", result.blocks, result.uncles, result.orphans)

	for _, block := range result.orphanedBlocks {
		err = u.backend.WriteOrphan(block)
		if err != nil {
			u.halt = true
			u.lastFail = err
			log.Printf("Failed to insert orphaned block into backend: %v", err)
			return
		}
	}
	log.Printf("Inserted %v orphaned blocks to backend", result.orphans)

	totalRevenue := new(big.Rat)
	totalMinersProfit := new(big.Rat)
	totalPoolProfit := new(big.Rat)

	for _, block := range result.maturedBlocks {
		revenue, minersProfit, poolProfit, roundRewards, err := u.calculateRewards(block)
		if err != nil {
			u.halt = true
			u.lastFail = err
			log.Printf("Failed to calculate rewards for round %v: %v", block.RoundKey(), err)
			return
		}
		err = u.backend.WriteMaturedBlock(block, roundRewards)
		if err != nil {
			u.halt = true
			u.lastFail = err
			log.Printf("Failed to credit rewards for round %v: %v", block.RoundKey(), err)
			return
		}
		totalRevenue.Add(totalRevenue, revenue)
		totalMinersProfit.Add(totalMinersProfit, minersProfit)
		totalPoolProfit.Add(totalPoolProfit, poolProfit)

		logEntry := fmt.Sprintf(
			"MATURED %v: revenue %v, miners profit %v, pool profit: %v",
			block.RoundKey(),
			util.FormatRatReward(revenue),
			util.FormatRatReward(minersProfit),
			util.FormatRatReward(poolProfit),
		)
		entries := []string{logEntry}
		for login, reward := range roundRewards {
			entries = append(entries, fmt.Sprintf("\tREWARD %v: %v: %v Shannon", block.RoundKey(), login, reward))
		}
		log.Println(strings.Join(entries, "\n"))
	}

	log.Printf(
		"MATURE SESSION: revenue %v, miners profit %v, pool profit: %v",
		util.FormatRatReward(totalRevenue),
		util.FormatRatReward(totalMinersProfit),
		util.FormatRatReward(totalPoolProfit),
	)
}

func (u *BlockUnlocker) calculateRewards(block *storage.BlockData) (*big.Rat, *big.Rat, *big.Rat, map[string]int64, error) {
	revenue := new(big.Rat).SetInt(block.Reward)
	minersProfit, poolProfit := chargeFee(revenue, u.config.PoolFee)

	shares, err := u.backend.GetRoundShares(block.RoundHeight, block.Nonce)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	rewards := calculateRewardsForShares(shares, block.TotalShares, minersProfit)

	if block.ExtraReward != nil {
		extraReward := new(big.Rat).SetInt(block.ExtraReward)
		poolProfit.Add(poolProfit, extraReward)
		revenue.Add(revenue, extraReward)
	}

	if len(u.config.PoolFeeAddress) != 0 {
		address := strings.ToLower(u.config.PoolFeeAddress)
		rewards[address] += weiToShannonInt64(poolProfit)
	}

	return revenue, minersProfit, poolProfit, rewards, nil
}

func calculateRewardsForShares(shares map[string]int64, total int64, reward *big.Rat) map[string]int64 {
	rewards := make(map[string]int64)

	for login, n := range shares {
		percent := big.NewRat(n, total)
		workerReward := new(big.Rat).Mul(reward, percent)
		rewards[login] += weiToShannonInt64(workerReward)
	}
	return rewards
}

// Returns new value after fee deduction and fee value.
func chargeFee(value *big.Rat, fee float64) (*big.Rat, *big.Rat) {
	feePercent := new(big.Rat).SetFloat64(fee / 100)
	feeValue := new(big.Rat).Mul(value, feePercent)
	return new(big.Rat).Sub(value, feeValue), feeValue
}

func weiToShannonInt64(wei *big.Rat) int64 {
	shannon := new(big.Rat).SetInt(util.Shannon)
	inShannon := new(big.Rat).Quo(wei, shannon)
	value, _ := strconv.ParseInt(inShannon.FloatString(0), 10, 64)
	return value
}

func GetBlockWinnerRewardByEra(era *big.Int, blockReward *big.Int) *big.Int {
	if era.Cmp(big.NewInt(0)) == 0 {
		return new(big.Int).Set(blockReward)
	}

	var q, d, r *big.Int = new(big.Int), new(big.Int), new(big.Int)

	q.Exp(disinflationRateQuotient, era, nil)
	d.Exp(disinflationRateDivisor, era, nil)

	r.Mul(blockReward, q)
	r.Div(r, d)

	return r
}

func GetBlockEra(blockNum, eraLength *big.Int) *big.Int {

	if blockNum.Sign() < 1 {
		return new(big.Int)
	}

	remainder := big.NewInt(0).Mod(big.NewInt(0).Sub(blockNum, big.NewInt(1)), eraLength)
	base := big.NewInt(0).Sub(blockNum, remainder)

	d := big.NewInt(0).Div(base, eraLength)
	dremainder := big.NewInt(0).Mod(d, big.NewInt(1))

	return new(big.Int).Sub(d, dremainder)
}

func getConstReward(era *big.Int) *big.Int {
	var blockReward = homesteadReward
	wr := GetBlockWinnerRewardByEra(era, blockReward)
	return wr
}

func getRewardForUncle(blockReward *big.Int) *big.Int {
	return new(big.Int).Div(blockReward, big32)
}

func getUncleReward(uHeight *big.Int, height *big.Int, era *big.Int, reward *big.Int) *big.Int {
	if era.Cmp(big.NewInt(0)) == 0 {
		r := new(big.Int)
		r.Add(uHeight, big8)
		r.Sub(r, height)
		r.Mul(r, reward)
		r.Div(r, big8)
		return r
	}
	return getRewardForUncle(reward)
}

func (u *BlockUnlocker) getExtraRewardForTx(block *rpc.GetBlockReply) (*big.Int, error) {
	amount := new(big.Int)

	for _, tx := range block.Transactions {
		receipt, err := u.rpc.GetTxReceipt(tx.Hash)
		if err != nil {
			return nil, err
		}
		if receipt != nil {
			gasUsed := util.String2Big(receipt.GasUsed)
			gasPrice := util.String2Big(tx.GasPrice)
			fee := new(big.Int).Mul(gasUsed, gasPrice)
			amount.Add(amount, fee)
		}
	}
	return amount, nil
}
