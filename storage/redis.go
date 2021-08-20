package storage

import (
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/redis.v3"
	"github.com/cyberpoolorg/etc-stratum/util"
)

type Config struct {
	Endpoint string `json:"endpoint"`
	Password string `json:"password"`
	Database int64  `json:"database"`
	PoolSize int    `json:"poolSize"`
}

type RedisClient struct {
	client *redis.Client
	prefix string
}

type PoolCharts struct {
	Timestamp  int64  `json:"x"`
	TimeFormat string `json:"timeFormat"`
	PoolHash   int64  `json:"y"`
}

type MinerCharts struct {
	Timestamp      int64  `json:"x"`
	TimeFormat     string `json:"timeFormat"`
	MinerHash      int64  `json:"minerHash"`
	MinerLargeHash int64  `json:"minerLargeHash"`
	WorkerOnline   string `json:"workerOnline"`
}

type PaymentCharts struct {
	Timestamp  int64  `json:"x"`
	TimeFormat string `json:"timeFormat"`
	Amount     int64  `json:"amount"`
}

type LuckCharts struct {
	Timestamp  int64   `json:"x"`
	Height     int64   `json:"height"`
	Difficulty int64   `json:"difficulty"`
	Shares     int64   `json:"shares"`
	SharesDiff float64 `json:"sharesDiff"`
	Reward     string  `json:"reward"`
}

type NetCharts struct {
	Timestamp  int64  `json:"x"`
	TimeFormat string `json:"timeFormat"`
	NetHash    int64  `json:"y"`
}

type ClientCharts struct {
	Timestamp  int64  `json:"x"`
	TimeFormat string `json:"timeFormat"`
	ClientTot  int64  `json:"y"`
}
type BlockData struct {
	Height         int64    `json:"height"`
	Timestamp      int64    `json:"timestamp"`
	Difficulty     int64    `json:"difficulty"`
	TotalShares    int64    `json:"shares"`
	Uncle          bool     `json:"uncle"`
	UncleHeight    int64    `json:"uncleHeight"`
	Orphan         bool     `json:"orphan"`
	Hash           string   `json:"hash"`
	Nonce          string   `json:"-"`
	PowHash        string   `json:"-"`
	MixDigest      string   `json:"-"`
	Reward         *big.Int `json:"-"`
	ExtraReward    *big.Int `json:"-"`
	ImmatureReward string   `json:"-"`
	RewardString   string   `json:"reward"`
	RoundHeight    int64    `json:"-"`
	candidateKey   string
	immatureKey    string
}

func (b *BlockData) RewardInShannon() int64 {
	reward := new(big.Int).Div(b.Reward, util.Shannon)
	return reward.Int64()
}

func (b *BlockData) serializeHash() string {
	if len(b.Hash) > 0 {
		return b.Hash
	} else {
		return "0x0"
	}
}

func (b *BlockData) RoundKey() string {
	return join(b.RoundHeight, b.Hash)
}

func (b *BlockData) key() string {
	return join(b.UncleHeight, b.Orphan, b.Nonce, b.serializeHash(), b.Timestamp, b.Difficulty, b.TotalShares, b.Reward)
}

type Miner struct {
	LastBeat  int64 `json:"lastBeat"`
	HR        int64 `json:"hr"`
	Offline   bool  `json:"offline"`
	startedAt int64
}

type Worker struct {
	Miner
	TotalHR int64 `json:"hr2"`
}

func NewRedisClient(cfg *Config, prefix string) *RedisClient {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Endpoint,
		Password: cfg.Password,
		DB:       cfg.Database,
		PoolSize: cfg.PoolSize,
	})
	return &RedisClient{client: client, prefix: prefix}
}

func (r *RedisClient) Client() *redis.Client {
	return r.client
}

func (r *RedisClient) Check() (string, error) {
	return r.client.Ping().Result()
}

func (r *RedisClient) BgSave() (string, error) {
	return r.client.BgSave().Result()
}

func (r *RedisClient) GetBlacklist() ([]string, error) {
	cmd := r.client.SMembers(r.formatKey("blacklist"))
	if cmd.Err() != nil {
		return []string{}, cmd.Err()
	}
	return cmd.Val(), nil
}

func (r *RedisClient) GetWhitelist() ([]string, error) {
	cmd := r.client.SMembers(r.formatKey("whitelist"))
	if cmd.Err() != nil {
		return []string{}, cmd.Err()
	}
	return cmd.Val(), nil
}

func (r *RedisClient) WritePoolCharts(time1 int64, time2 string, poolHash string) error {
	s := join(time1, time2, poolHash)
	cmd := r.client.ZAdd(r.formatKey("charts", "pool"), redis.Z{Score: float64(time1), Member: s})
	return cmd.Err()
}

func (r *RedisClient) WriteMinerCharts(time1 int64, time2, k string, hash, largeHash, workerOnline int64) error {
	s := join(time1, time2, hash, largeHash, workerOnline)
	cmd := r.client.ZAdd(r.formatKey("charts", "miner", k), redis.Z{Score: float64(time1), Member: s})
	return cmd.Err()
}

func (r *RedisClient) WriteDiffCharts(time1 int64, time2 string, netHash string) error {
	s := join(time1, time2, netHash)
	cmd := r.client.ZAdd(r.formatKey("charts", "difficulty"), redis.Z{Score: float64(time1), Member: s})
	return cmd.Err()
}

func (r *RedisClient) WriteClientCharts(time1 int64, time2 string, clientTot string) error {
	s := join(time1, time2, clientTot)
	cmd := r.client.ZAdd(r.formatKey("charts", "client"), redis.Z{Score: float64(time1), Member: s})
	return cmd.Err()
}

func (r *RedisClient) GetPoolCharts(poolHashLen int64) (stats []*PoolCharts, err error) {

	tx := r.client.Multi()
	defer tx.Close()

	now := util.MakeTimestamp() / 1000

	cmds, err := tx.Exec(func() error {
		tx.ZRemRangeByScore(r.formatKey("charts", "pool"), "-inf", fmt.Sprint("(", now-172800))
		tx.ZRevRangeWithScores(r.formatKey("charts", "pool"), 0, poolHashLen)
		return nil
	})

	if err != nil {
		return nil, err
	}

	stats = convertPoolChartsResults(cmds[1].(*redis.ZSliceCmd))
	return stats, nil
}

func convertPoolChartsResults(raw *redis.ZSliceCmd) []*PoolCharts {
	var result []*PoolCharts
	for _, v := range raw.Val() {
		pc := PoolCharts{}
		pc.Timestamp = int64(v.Score)
		str := v.Member.(string)
		pc.TimeFormat = str[strings.Index(str, ":")+1 : strings.LastIndex(str, ":")]
		pc.PoolHash, _ = strconv.ParseInt(str[strings.LastIndex(str, ":")+1:], 10, 64)
		result = append(result, &pc)
	}
	var reverse []*PoolCharts
	for i := len(result) - 1; i >= 0; i-- {
		reverse = append(reverse, result[i]);
	}
	return reverse
}

func convertMinerChartsResults(raw *redis.ZSliceCmd) []*MinerCharts {
	var result []*MinerCharts
	for _, v := range raw.Val() {
		mc := MinerCharts{}
		mc.Timestamp = int64(v.Score)
		str := v.Member.(string)
		mc.TimeFormat = strings.Split(str, ":")[1]
		mc.MinerHash, _ = strconv.ParseInt(strings.Split(str, ":")[2], 10, 64)
		mc.MinerLargeHash, _ = strconv.ParseInt(strings.Split(str, ":")[3], 10, 64)
		mc.WorkerOnline = strings.Split(str, ":")[4]
		result = append(result, &mc)
	}
	var reverse []*MinerCharts
	for i := len(result) - 1; i >= 0; i-- {
		reverse = append(reverse, result[i]);
	}
	return reverse
}

func (r *RedisClient) GetAllMinerAccount() (account []string, err error) {
	var c int64
	for {
		now := util.MakeTimestamp() / 1000
		c, keys, err := r.client.Scan(c, r.formatKey("miners", "*"), now).Result()

		if err != nil {
			return account, err
		}
		for _, key := range keys {
			m := strings.Split(key, ":")

			if len(m) >= 2 {
				account = append(account, m[2])
			}
		}
		if c == 0 {
			break
		}
	}
	return account, nil
}

func (r *RedisClient) GetMinerCharts(hashNum int64, login string) (stats []*MinerCharts, err error) {

	tx := r.client.Multi()
	defer tx.Close()
	now := util.MakeTimestamp() / 1000
	cmds, err := tx.Exec(func() error {
		tx.ZRemRangeByScore(r.formatKey("charts", "miner", login), "-inf", fmt.Sprint("(", now-172800))
		tx.ZRevRangeWithScores(r.formatKey("charts", "miner", login), 0, hashNum)
		return nil
	})
	if err != nil {
		return nil, err
	}
	stats = convertMinerChartsResults(cmds[1].(*redis.ZSliceCmd))
	return stats, nil
}

func (r *RedisClient) GetPaymentCharts(login string) (stats []*PaymentCharts, err error) {

	tx := r.client.Multi()
	defer tx.Close()
	cmds, err := tx.Exec(func() error {
		tx.ZRevRangeWithScores(r.formatKey("payments", login), 0, 360)
		return nil
	})
	if err != nil {
		return nil, err
	}
	stats = convertPaymentChartsResults(cmds[0].(*redis.ZSliceCmd))
	return stats, nil
}

func convertPaymentChartsResults(raw *redis.ZSliceCmd) []*PaymentCharts {
	var result []*PaymentCharts
	for _, v := range raw.Val() {
		pc := PaymentCharts{}
		pc.Timestamp = int64(v.Score)
		tm := time.Unix(pc.Timestamp, 0)
		pc.TimeFormat = tm.Format("2006-01-02") + " 00_00"
		fields := strings.Split(v.Member.(string), ":")
		pc.Amount, _ = strconv.ParseInt(fields[1], 10, 64)

		var chkAppend bool
		for _, pcc := range result {
			if pcc.TimeFormat == pc.TimeFormat {
				pcc.Amount += pc.Amount
				chkAppend = true
			}
		}
		if !chkAppend {
			pc.Timestamp -= int64(math.Mod(float64(v.Score), float64(86400)))
			result = append(result, &pc)
		}
	}
	var reverse []*PaymentCharts
	for i := len(result) - 1; i >= 0; i-- {
		reverse = append(reverse, result[i]);
	}
	return reverse
}

func (r *RedisClient) GetNetCharts(netHashLen int64) (stats []*NetCharts, err error) {

	tx := r.client.Multi()
	defer tx.Close()

	now := util.MakeTimestamp() / 1000

	cmds, err := tx.Exec(func() error {
		tx.ZRemRangeByScore(r.formatKey("charts", "difficulty"), "-inf", fmt.Sprint("(", now-172800))
		tx.ZRevRangeWithScores(r.formatKey("charts", "difficulty"), 0, netHashLen)
		return nil
	})

	if err != nil {
		return nil, err
	}

	stats = convertNetChartsResults(cmds[1].(*redis.ZSliceCmd))
	return stats, nil
}

func convertNetChartsResults(raw *redis.ZSliceCmd) []*NetCharts {
	var result []*NetCharts
	for _, v := range raw.Val() {

		nc := NetCharts{}
		nc.Timestamp = int64(v.Score)
		str := v.Member.(string)
		nc.TimeFormat = str[strings.Index(str, ":")+1 : strings.LastIndex(str, ":")]
		nc.NetHash, _ = strconv.ParseInt(str[strings.LastIndex(str, ":")+1:], 10, 64)
		result = append(result, &nc)
	}

	var reverse []*NetCharts
	for i := len(result) - 1; i >= 0; i-- {
		reverse = append(reverse, result[i])
	}
	return reverse
}

func (r *RedisClient) GetClientCharts(clientTotLen int64) (stats []*ClientCharts, err error) {

	tx := r.client.Multi()
	defer tx.Close()

	now := util.MakeTimestamp() / 1000

	cmds, err := tx.Exec(func() error {
		tx.ZRemRangeByScore(r.formatKey("charts", "client"), "-inf", fmt.Sprint("(", now-172800))
		tx.ZRevRangeWithScores(r.formatKey("charts", "client"), 0, clientTotLen)
		return nil
	})

	if err != nil {
		return nil, err
	}

	stats = convertClientChartsResults(cmds[1].(*redis.ZSliceCmd))
	return stats, nil
}

func convertClientChartsResults(raw *redis.ZSliceCmd) []*ClientCharts {
	var result []*ClientCharts
	for _, v := range raw.Val() {

		cc := ClientCharts{}
		cc.Timestamp = int64(v.Score)
		str := v.Member.(string)
		cc.TimeFormat = str[strings.Index(str, ":")+1 : strings.LastIndex(str, ":")]
		cc.ClientTot, _ = strconv.ParseInt(str[strings.LastIndex(str, ":")+1:], 10, 64)
		result = append(result, &cc)
	}

	var reverse []*ClientCharts
	for i := len(result) - 1; i >= 0; i-- {
		reverse = append(reverse, result[i])
	}
	return reverse
}

func (r *RedisClient) WriteNodeState(id string, height uint64, diff *big.Int, blocktime float64) error {
	tx := r.client.Multi()
	defer tx.Close()

	now := util.MakeTimestamp() / 1000

	_, err := tx.Exec(func() error {
		tx.HSet(r.formatKey("nodes"), join(id, "name"), id)
		tx.HSet(r.formatKey("nodes"), join(id, "height"), strconv.FormatUint(height, 10))
		tx.HSet(r.formatKey("nodes"), join(id, "difficulty"), diff.String())
		tx.HSet(r.formatKey("nodes"), join(id, "lastBeat"), strconv.FormatInt(now, 10))
		tx.HSet(r.formatKey("nodes"), join(id, "blocktime"), strconv.FormatFloat(blocktime, 'f', 4, 64))
		return nil
	})
	return err
}

func (r *RedisClient) GetNodeStates() ([]map[string]interface{}, error) {
	cmd := r.client.HGetAllMap(r.formatKey("nodes"))
	if cmd.Err() != nil {
		return nil, cmd.Err()
	}
	m := make(map[string]map[string]interface{})
	for key, value := range cmd.Val() {
		parts := strings.Split(key, ":")
		if val, ok := m[parts[0]]; ok {
			val[parts[1]] = value
		} else {
			node := make(map[string]interface{})
			node[parts[1]] = value
			m[parts[0]] = node
		}
	}
	v := make([]map[string]interface{}, len(m), len(m))
	i := 0
	for _, value := range m {
		v[i] = value
		i++
	}
	return v, nil
}

func (r *RedisClient) checkPoWExist(height uint64, params []string) (bool, error) {

	r.client.ZRemRangeByScore(r.formatKey("pow"), "-inf", fmt.Sprint("(", height-8))
	val, err := r.client.ZAdd(r.formatKey("pow"), redis.Z{Score: float64(height), Member: strings.Join(params, ":")}).Result()
	return val == 0, err
}

func (r *RedisClient) WriteShare(login, id string, params []string, diff int64, height uint64, window time.Duration) (bool, error) {
	exist, err := r.checkPoWExist(height, params)
	if err != nil {
		return false, err
	}

	if exist {
		return true, nil
	}
	tx := r.client.Multi()
	defer tx.Close()

	ms := util.MakeTimestamp()
	ts := ms / 1000

	_, err = tx.Exec(func() error {
		r.writeShare(tx, ms, ts, login, id, diff, window)
		tx.HIncrBy(r.formatKey("stats"), "roundShares", diff)
		return nil
	})
	return false, err
}

func (r *RedisClient) WriteBlock(login, id string, params []string, diff, roundDiff int64, height uint64, window time.Duration) (bool, error) {
	exist, err := r.checkPoWExist(height, params)
	if err != nil {
		return false, err
	}

	if exist {
		return true, nil
	}
	tx := r.client.Multi()
	defer tx.Close()

	ms := util.MakeTimestamp()
	ts := ms / 1000

	cmds, err := tx.Exec(func() error {
		r.writeShare(tx, ms, ts, login, id, diff, window)
		tx.HSet(r.formatKey("stats"), "lastBlockFound", strconv.FormatInt(ts, 10))
		tx.HDel(r.formatKey("stats"), "roundShares")
		tx.ZIncrBy(r.formatKey("finders"), 1, login)
		tx.HIncrBy(r.formatKey("miners", login), "blocksFound", 1)
		tx.Rename(r.formatKey("shares", "roundCurrent"), r.formatRound(int64(height), params[0]))
		tx.HGetAllMap(r.formatRound(int64(height), params[0]))
		return nil
	})
	if err != nil {
		return false, err
	} else {
		sharesMap, _ := cmds[10].(*redis.StringStringMapCmd).Result()
		totalShares := int64(0)
		for _, v := range sharesMap {
			n, _ := strconv.ParseInt(v, 10, 64)
			totalShares += n
		}
		hashHex := strings.Join(params, ":")
		s := join(hashHex, ts, roundDiff, totalShares)
		cmd := r.client.ZAdd(r.formatKey("blocks", "candidates"), redis.Z{Score: float64(height), Member: s})
		return false, cmd.Err()
	}
}

func (r *RedisClient) writeShare(tx *redis.Multi, ms, ts int64, login, id string, diff int64, expire time.Duration) {
	tx.HIncrBy(r.formatKey("shares", "roundCurrent"), login, diff)
	tx.ZAdd(r.formatKey("hashrate"), redis.Z{Score: float64(ts), Member: join(diff, login, id, ms)})
	tx.ZAdd(r.formatKey("hashrate", login), redis.Z{Score: float64(ts), Member: join(diff, id, ms)})
	tx.Expire(r.formatKey("hashrate", login), expire) // Will delete hashrates for miners that gone
	tx.HSet(r.formatKey("miners", login), "lastShare", strconv.FormatInt(ts, 10))
}

func (r *RedisClient) formatKey(args ...interface{}) string {
	return join(r.prefix, join(args...))
}

func (r *RedisClient) formatRound(height int64, nonce string) string {
	return r.formatKey("shares", "round"+strconv.FormatInt(height, 10), nonce)
}

func join(args ...interface{}) string {
	s := make([]string, len(args))
	for i, v := range args {
		switch v.(type) {
		case string:
			s[i] = v.(string)
		case int64:
			s[i] = strconv.FormatInt(v.(int64), 10)
		case uint64:
			s[i] = strconv.FormatUint(v.(uint64), 10)
		case float64:
			s[i] = strconv.FormatFloat(v.(float64), 'f', 0, 64)
		case bool:
			if v.(bool) {
				s[i] = "1"
			} else {
				s[i] = "0"
			}
		case *big.Int:
			n := v.(*big.Int)
			if n != nil {
				s[i] = n.String()
			} else {
				s[i] = "0"
			}
		default:
			panic("Invalid type specified for conversion")
		}
	}
	return strings.Join(s, ":")
}

func (r *RedisClient) GetCandidates(maxHeight int64) ([]*BlockData, error) {
	option := redis.ZRangeByScore{Min: "0", Max: strconv.FormatInt(maxHeight, 10)}
	cmd := r.client.ZRangeByScoreWithScores(r.formatKey("blocks", "candidates"), option)
	if cmd.Err() != nil {
		return nil, cmd.Err()
	}
	return convertCandidateResults(cmd), nil
}

func (r *RedisClient) GetImmatureBlocks(maxHeight int64) ([]*BlockData, error) {
	option := redis.ZRangeByScore{Min: "0", Max: strconv.FormatInt(maxHeight, 10)}
	cmd := r.client.ZRangeByScoreWithScores(r.formatKey("blocks", "immature"), option)
	if cmd.Err() != nil {
		return nil, cmd.Err()
	}
	return convertBlockResults(cmd), nil
}

func (r *RedisClient) GetRoundShares(height int64, nonce string) (map[string]int64, error) {
	result := make(map[string]int64)
	cmd := r.client.HGetAllMap(r.formatRound(height, nonce))
	if cmd.Err() != nil {
		return nil, cmd.Err()
	}
	sharesMap, _ := cmd.Result()
	for login, v := range sharesMap {
		n, _ := strconv.ParseInt(v, 10, 64)
		result[login] = n
	}
	return result, nil
}

func (r *RedisClient) GetPayees() ([]string, error) {
	payees := make(map[string]struct{})
	var result []string
	var c int64

	for {
		var keys []string
		var err error
		c, keys, err = r.client.Scan(c, r.formatKey("miners", "*"), 100).Result()
		if err != nil {
			return nil, err
		}
		for _, row := range keys {
			login := strings.Split(row, ":")[2]
			payees[login] = struct{}{}
		}
		if c == 0 {
			break
		}
	}
	for login, _ := range payees {
		result = append(result, login)
	}
	return result, nil
}

func (r *RedisClient) GetBalance(login string) (int64, error) {
	cmd := r.client.HGet(r.formatKey("miners", login), "balance")
	if cmd.Err() == redis.Nil {
		return 0, nil
	} else if cmd.Err() != nil {
		return 0, cmd.Err()
	}
	return cmd.Int64()
}

func (r *RedisClient) LockPayouts(login string, amount int64) error {
	key := r.formatKey("payments", "lock")
	result := r.client.SetNX(key, join(login, amount), 0).Val()
	if !result {
		return fmt.Errorf("Unable to acquire lock '%s'", key)
	}
	return nil
}

func (r *RedisClient) UnlockPayouts() error {
	key := r.formatKey("payments", "lock")
	_, err := r.client.Del(key).Result()
	return err
}

func (r *RedisClient) IsPayoutsLocked() (bool, error) {
	_, err := r.client.Get(r.formatKey("payments", "lock")).Result()
	if err == redis.Nil {
		return false, nil
	} else if err != nil {
		return false, err
	} else {
		return true, nil
	}
}

type PendingPayment struct {
	Timestamp int64  `json:"timestamp"`
	Amount    int64  `json:"amount"`
	Address   string `json:"login"`
}

func (r *RedisClient) GetPendingPayments() []*PendingPayment {
	raw := r.client.ZRevRangeWithScores(r.formatKey("payments", "pending"), 0, -1)
	var result []*PendingPayment
	for _, v := range raw.Val() {

		payment := PendingPayment{}
		payment.Timestamp = int64(v.Score)
		fields := strings.Split(v.Member.(string), ":")
		payment.Address = fields[0]
		payment.Amount, _ = strconv.ParseInt(fields[1], 10, 64)
		result = append(result, &payment)
	}
	return result
}

func (r *RedisClient) UpdateBalance(login string, amount int64) error {
	tx := r.client.Multi()
	defer tx.Close()

	ts := util.MakeTimestamp() / 1000

	_, err := tx.Exec(func() error {
		tx.HIncrBy(r.formatKey("miners", login), "balance", (amount * -1))
		tx.HIncrBy(r.formatKey("miners", login), "pending", amount)
		tx.HIncrBy(r.formatKey("finances"), "balance", (amount * -1))
		tx.HIncrBy(r.formatKey("finances"), "pending", amount)
		tx.ZAdd(r.formatKey("payments", "pending"), redis.Z{Score: float64(ts), Member: join(login, amount)})
		return nil
	})
	return err
}

func (r *RedisClient) RollbackBalance(login string, amount int64) error {
	tx := r.client.Multi()
	defer tx.Close()

	_, err := tx.Exec(func() error {
		tx.HIncrBy(r.formatKey("miners", login), "balance", amount)
		tx.HIncrBy(r.formatKey("miners", login), "pending", (amount * -1))
		tx.HIncrBy(r.formatKey("finances"), "balance", amount)
		tx.HIncrBy(r.formatKey("finances"), "pending", (amount * -1))
		tx.ZRem(r.formatKey("payments", "pending"), join(login, amount))
		return nil
	})
	return err
}

func (r *RedisClient) WritePayment(login, txHash string, amount int64) error {
	tx := r.client.Multi()
	defer tx.Close()

	ts := util.MakeTimestamp() / 1000

	_, err := tx.Exec(func() error {
		tx.HIncrBy(r.formatKey("miners", login), "pending", (amount * -1))
		tx.HIncrBy(r.formatKey("miners", login), "paid", amount)
		tx.HIncrBy(r.formatKey("finances"), "pending", (amount * -1))
		tx.HIncrBy(r.formatKey("finances"), "paid", amount)
		tx.ZAdd(r.formatKey("payments", "all"), redis.Z{Score: float64(ts), Member: join(txHash, login, amount)})
		tx.ZAdd(r.formatKey("payments", login), redis.Z{Score: float64(ts), Member: join(txHash, amount)})
		tx.ZRem(r.formatKey("payments", "pending"), join(login, amount))
		tx.Del(r.formatKey("payments", "lock"))
		return nil
	})
	return err
}

func (r *RedisClient) WriteImmatureBlock(block *BlockData, roundRewards map[string]int64) error {
	tx := r.client.Multi()
	defer tx.Close()

	_, err := tx.Exec(func() error {
		r.writeImmatureBlock(tx, block)
		total := int64(0)
		for login, amount := range roundRewards {
			total += amount
			tx.HIncrBy(r.formatKey("miners", login), "immature", amount)
			tx.HSetNX(r.formatKey("credits", "immature", block.Height, block.Hash), login, strconv.FormatInt(amount, 10))
		}
		tx.HIncrBy(r.formatKey("finances"), "immature", total)
		return nil
	})
	return err
}

func (r *RedisClient) WriteMaturedBlock(block *BlockData, roundRewards map[string]int64) error {
	creditKey := r.formatKey("credits", "immature", block.RoundHeight, block.Hash)
	tx, err := r.client.Watch(creditKey)

	immatureCredits := tx.HGetAllMap(creditKey)
	if err != nil {
		return err
	}
	defer tx.Close()

	ts := util.MakeTimestamp() / 1000
	value := join(block.Hash, ts, block.Reward)

	_, err = tx.Exec(func() error {
		r.writeMaturedBlock(tx, block)
		tx.ZAdd(r.formatKey("credits", "all"), redis.Z{Score: float64(block.Height), Member: value})

		totalImmature := int64(0)
		for login, amountString := range immatureCredits.Val() {
			amount, _ := strconv.ParseInt(amountString, 10, 64)
			totalImmature += amount
			tx.HIncrBy(r.formatKey("miners", login), "immature", (amount * -1))
		}

		total := int64(0)
		for login, amount := range roundRewards {
			total += amount
			tx.HIncrBy(r.formatKey("miners", login), "balance", amount)
			tx.HSetNX(r.formatKey("credits", block.Height, block.Hash), login, strconv.FormatInt(amount, 10))
		}
		tx.Del(creditKey)
		tx.HIncrBy(r.formatKey("finances"), "balance", total)
		tx.HIncrBy(r.formatKey("finances"), "immature", (totalImmature * -1))
		tx.HSet(r.formatKey("finances"), "lastCreditHeight", strconv.FormatInt(block.Height, 10))
		tx.HSet(r.formatKey("finances"), "lastCreditHash", block.Hash)
		tx.HIncrBy(r.formatKey("finances"), "totalMined", block.RewardInShannon())
		return nil
	})
	return err
}

func (r *RedisClient) WriteOrphan(block *BlockData) error {
	creditKey := r.formatKey("credits", "immature", block.RoundHeight, block.Hash)
	tx, err := r.client.Watch(creditKey)

	immatureCredits := tx.HGetAllMap(creditKey)
	if err != nil {
		return err
	}
	defer tx.Close()

	_, err = tx.Exec(func() error {
		r.writeMaturedBlock(tx, block)

		totalImmature := int64(0)
		for login, amountString := range immatureCredits.Val() {
			amount, _ := strconv.ParseInt(amountString, 10, 64)
			totalImmature += amount
			tx.HIncrBy(r.formatKey("miners", login), "immature", (amount * -1))
		}
		tx.Del(creditKey)
		tx.HIncrBy(r.formatKey("finances"), "immature", (totalImmature * -1))
		return nil
	})
	return err
}

func (r *RedisClient) WritePendingOrphans(blocks []*BlockData) error {
	tx := r.client.Multi()
	defer tx.Close()

	_, err := tx.Exec(func() error {
		for _, block := range blocks {
			r.writeImmatureBlock(tx, block)
		}
		return nil
	})
	return err
}

func (r *RedisClient) writeImmatureBlock(tx *redis.Multi, block *BlockData) {

	if block.Height != block.RoundHeight {
		tx.Rename(r.formatRound(block.RoundHeight, block.Nonce), r.formatRound(block.Height, block.Nonce))
	}
	tx.ZRem(r.formatKey("blocks", "candidates"), block.candidateKey)
	tx.ZAdd(r.formatKey("blocks", "immature"), redis.Z{Score: float64(block.Height), Member: block.key()})
}

func (r *RedisClient) writeMaturedBlock(tx *redis.Multi, block *BlockData) {
	tx.Del(r.formatRound(block.RoundHeight, block.Nonce))
	tx.ZRem(r.formatKey("blocks", "immature"), block.immatureKey)
	tx.ZAdd(r.formatKey("blocks", "matured"), redis.Z{Score: float64(block.Height), Member: block.key()})
}

func (r *RedisClient) IsMinerExists(login string) (bool, error) {
	return r.client.Exists(r.formatKey("miners", login)).Result()
}

func (r *RedisClient) GetMinerStats(login string, maxPayments int64) (map[string]interface{}, error) {
	stats := make(map[string]interface{})

	tx := r.client.Multi()
	defer tx.Close()

	cmds, err := tx.Exec(func() error {
		tx.HGetAllMap(r.formatKey("miners", login))
		tx.ZRevRangeWithScores(r.formatKey("payments", login), 0, maxPayments-1)
		tx.ZCard(r.formatKey("payments", login))
		tx.HGet(r.formatKey("shares", "roundCurrent"), login)
		return nil
	})

	if err != nil && err != redis.Nil {
		return nil, err
	} else {
		result, _ := cmds[0].(*redis.StringStringMapCmd).Result()
		stats["stats"] = convertStringMap(result)
		payments := convertPaymentsResults(cmds[1].(*redis.ZSliceCmd))
		stats["payments"] = payments
		stats["paymentsTotal"] = cmds[2].(*redis.IntCmd).Val()
		roundShares, _ := cmds[3].(*redis.StringCmd).Int64()
		stats["roundShares"] = roundShares
	}

	return stats, nil
}

func convertStringMap(m map[string]string) map[string]interface{} {
	result := make(map[string]interface{})
	var err error
	for k, v := range m {
		result[k], err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			result[k] = v
		}
	}
	return result
}

func (r *RedisClient) FlushStaleStats(window, largeWindow time.Duration) (int64, error) {
	now := util.MakeTimestamp() / 1000
	max := fmt.Sprint("(", now-int64(window/time.Second))
	total, err := r.client.ZRemRangeByScore(r.formatKey("hashrate"), "-inf", max).Result()
	if err != nil {
		return total, err
	}

	var c int64
	miners := make(map[string]struct{})
	max = fmt.Sprint("(", now-int64(largeWindow/time.Second))

	for {
		var keys []string
		var err error
		c, keys, err = r.client.Scan(c, r.formatKey("hashrate", "*"), 100).Result()
		if err != nil {
			return total, err
		}
		for _, row := range keys {
			login := strings.Split(row, ":")[2]
			if _, ok := miners[login]; !ok {
				n, err := r.client.ZRemRangeByScore(r.formatKey("hashrate", login), "-inf", max).Result()
				if err != nil {
					return total, err
				}
				miners[login] = struct{}{}
				total += n
			}
		}
		if c == 0 {
			break
		}
	}
	return total, nil
}

func (r *RedisClient) CollectStats(smallWindow time.Duration, maxBlocks, maxPayments int64) (map[string]interface{}, error) {
	window := int64(smallWindow / time.Second)
	stats := make(map[string]interface{})

	tx := r.client.Multi()
	defer tx.Close()

	now := util.MakeTimestamp() / 1000

	cmds, err := tx.Exec(func() error {
		tx.ZRemRangeByScore(r.formatKey("hashrate"), "-inf", fmt.Sprint("(", now-window))
		tx.ZRangeWithScores(r.formatKey("hashrate"), 0, -1)
		tx.HGetAllMap(r.formatKey("stats"))
		tx.ZRevRangeWithScores(r.formatKey("blocks", "candidates"), 0, -1)
		tx.ZRevRangeWithScores(r.formatKey("blocks", "immature"), 0, -1)
		tx.ZRevRangeWithScores(r.formatKey("blocks", "matured"), 0, maxBlocks-1)
		tx.ZCard(r.formatKey("blocks", "candidates"))
		tx.ZCard(r.formatKey("blocks", "immature"))
		tx.ZCard(r.formatKey("blocks", "matured"))
		tx.ZCard(r.formatKey("payments", "all"))
		tx.ZRevRangeWithScores(r.formatKey("payments", "all"), 0, maxPayments-1)
		return nil
	})

	if err != nil {
		return nil, err
	}

	result, _ := cmds[2].(*redis.StringStringMapCmd).Result()
	stats["stats"] = convertStringMap(result)
	candidates := convertCandidateResults(cmds[3].(*redis.ZSliceCmd))
	stats["candidates"] = candidates
	stats["candidatesTotal"] = cmds[6].(*redis.IntCmd).Val()

	immature := convertBlockResults(cmds[4].(*redis.ZSliceCmd))
	stats["immature"] = immature
	stats["immatureTotal"] = cmds[7].(*redis.IntCmd).Val()

	matured := convertBlockResults(cmds[5].(*redis.ZSliceCmd))
	stats["matured"] = matured
	stats["maturedTotal"] = cmds[8].(*redis.IntCmd).Val()

	payments := convertPaymentsResults(cmds[10].(*redis.ZSliceCmd))
	stats["payments"] = payments
	stats["paymentsTotal"] = cmds[9].(*redis.IntCmd).Val()

	totalHashrate, miners := convertMinersStats(window, cmds[1].(*redis.ZSliceCmd))
	stats["miners"] = miners
	stats["minersTotal"] = len(miners)
	stats["hashrate"] = totalHashrate

	workers := convertWorkersStats(window, cmds[1].(*redis.ZSliceCmd))
	stats["workers"] = workers
	stats["workersTotal"] = len(workers)

	return stats, nil
}

func (r *RedisClient) CollectWorkersStats(sWindow, lWindow time.Duration, login string) (map[string]interface{}, error) {
	smallWindow := int64(sWindow / time.Second)
	largeWindow := int64(lWindow / time.Second)
	stats := make(map[string]interface{})

	tx := r.client.Multi()
	defer tx.Close()

	now := util.MakeTimestamp() / 1000

	cmds, err := tx.Exec(func() error {
		tx.ZRemRangeByScore(r.formatKey("hashrate", login), "-inf", fmt.Sprint("(", now-largeWindow))
		tx.ZRangeWithScores(r.formatKey("hashrate", login), 0, -1)
		return nil
	})

	if err != nil {
		return nil, err
	}

	totalHashrate := int64(0)
	currentHashrate := int64(0)
	online := int64(0)
	offline := int64(0)
	workers := convertWorkersStats(smallWindow, cmds[1].(*redis.ZSliceCmd))

	for id, worker := range workers {
		timeOnline := now - worker.startedAt
		if timeOnline < 600 {
			timeOnline = 600
		}

		boundary := timeOnline
		if timeOnline >= smallWindow {
			boundary = smallWindow
		}
		worker.HR = worker.HR / boundary

		boundary = timeOnline
		if timeOnline >= largeWindow {
			boundary = largeWindow
		}
		worker.TotalHR = worker.TotalHR / boundary

		if worker.LastBeat < (now - smallWindow/2) {
			worker.Offline = true
			offline++
		} else {
			online++
		}

		currentHashrate += worker.HR
		totalHashrate += worker.TotalHR
		workers[id] = worker
	}
	stats["workers"] = workers
	stats["workersTotal"] = len(workers)
	stats["workersOnline"] = online
	stats["workersOffline"] = offline
	stats["hashrate"] = totalHashrate
	stats["currentHashrate"] = currentHashrate
	return stats, nil
}

func (r *RedisClient) CollectLuckStats(windows []int) (map[string]interface{}, error) {
	stats := make(map[string]interface{})

	tx := r.client.Multi()
	defer tx.Close()

	max := int64(windows[len(windows)-1])

	cmds, err := tx.Exec(func() error {
		tx.ZRevRangeWithScores(r.formatKey("blocks", "immature"), 0, -1)
		tx.ZRevRangeWithScores(r.formatKey("blocks", "matured"), 0, max-1)
		return nil
	})
	if err != nil {
		return stats, err
	}
	blocks := convertBlockResults(cmds[0].(*redis.ZSliceCmd), cmds[1].(*redis.ZSliceCmd))

	calcLuck := func(max int) (int, float64, float64, float64) {
		var total int
		var sharesDiff, uncles, orphans float64
		for i, block := range blocks {
			if i > (max - 1) {
				break
			}
			if block.Uncle {
				uncles++
			}
			if block.Orphan {
				orphans++
			}
			sharesDiff += float64(block.TotalShares) / float64(block.Difficulty)
			total++
		}
		if total > 0 {
			sharesDiff /= float64(total)
			uncles /= float64(total)
			orphans /= float64(total)
		}
		return total, sharesDiff, uncles, orphans
	}
	for _, max := range windows {
		total, sharesDiff, uncleRate, orphanRate := calcLuck(max)
		row := map[string]float64{
			"luck": sharesDiff, "uncleRate": uncleRate, "orphanRate": orphanRate,
		}
		stats[strconv.Itoa(total)] = row
		if total < max {
			break
		}
	}
	return stats, nil
}

func (r *RedisClient) CollectLuckCharts(max int) (stats []*LuckCharts, err error) {
	var result []*LuckCharts
	tx := r.client.Multi()
	defer tx.Close()

	cmds, err := tx.Exec(func() error {
		tx.ZRevRangeWithScores(r.formatKey("blocks", "matured"), 0, int64(max-1))
		return nil
	})
	if err != nil {
		return result, err
	}
	blocks := convertBlockResults(cmds[0].(*redis.ZSliceCmd))

	for i, block := range blocks {
		if i > (max - 1) {
			break
		}
		lc := LuckCharts{}
		var sharesDiff = float64(block.TotalShares) / float64(block.Difficulty)
		lc.Timestamp = block.Timestamp
		lc.Height = block.RoundHeight
		lc.Difficulty = block.Difficulty
		lc.Shares = block.TotalShares
		lc.SharesDiff = sharesDiff
		lc.Reward = block.RewardString
		result = append(result, &lc)
	}
	sort.Sort(TimestampSorter(result))
	return result, nil
}

type TimestampSorter []*LuckCharts

func (a TimestampSorter) Len() int           { return len(a) }
func (a TimestampSorter) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a TimestampSorter) Less(i, j int) bool { return a[i].Timestamp < a[j].Timestamp }

func convertCandidateResults(raw *redis.ZSliceCmd) []*BlockData {
	var result []*BlockData
	for _, v := range raw.Val() {

		block := BlockData{}
		block.Height = int64(v.Score)
		block.RoundHeight = block.Height
		fields := strings.Split(v.Member.(string), ":")
		block.Nonce = fields[0]
		block.PowHash = fields[1]
		block.MixDigest = fields[2]
		block.Timestamp, _ = strconv.ParseInt(fields[3], 10, 64)
		block.Difficulty, _ = strconv.ParseInt(fields[4], 10, 64)
		block.TotalShares, _ = strconv.ParseInt(fields[5], 10, 64)
		block.candidateKey = v.Member.(string)
		result = append(result, &block)
	}
	return result
}

func convertBlockResults(rows ...*redis.ZSliceCmd) []*BlockData {
	var result []*BlockData
	for _, row := range rows {
		for _, v := range row.Val() {

			block := BlockData{}
			block.Height = int64(v.Score)
			block.RoundHeight = block.Height
			fields := strings.Split(v.Member.(string), ":")
			block.UncleHeight, _ = strconv.ParseInt(fields[0], 10, 64)
			block.Uncle = block.UncleHeight > 0
			block.Orphan, _ = strconv.ParseBool(fields[1])
			block.Nonce = fields[2]
			block.Hash = fields[3]
			block.Timestamp, _ = strconv.ParseInt(fields[4], 10, 64)
			block.Difficulty, _ = strconv.ParseInt(fields[5], 10, 64)
			block.TotalShares, _ = strconv.ParseInt(fields[6], 10, 64)
			block.RewardString = fields[7]
			block.ImmatureReward = fields[7]
			block.immatureKey = v.Member.(string)
			result = append(result, &block)
		}
	}
	return result
}

func convertWorkersStats(window int64, raw *redis.ZSliceCmd) map[string]Worker {
	now := util.MakeTimestamp() / 1000
	workers := make(map[string]Worker)

	for _, v := range raw.Val() {
		parts := strings.Split(v.Member.(string), ":")
		share, _ := strconv.ParseInt(parts[0], 10, 64)
		id := parts[1]
		score := int64(v.Score)
		worker := workers[id]
		worker.TotalHR += share

		if score >= now-window {
			worker.HR += share
		}

		if worker.LastBeat < score {
			worker.LastBeat = score
		}
		if worker.startedAt > score || worker.startedAt == 0 {
			worker.startedAt = score
		}
		workers[id] = worker
	}
	return workers
}

func convertMinersStats(window int64, raw *redis.ZSliceCmd) (int64, map[string]Miner) {
	now := util.MakeTimestamp() / 1000
	miners := make(map[string]Miner)
	totalHashrate := int64(0)

	for _, v := range raw.Val() {
		parts := strings.Split(v.Member.(string), ":")
		share, _ := strconv.ParseInt(parts[0], 10, 64)
		id := parts[1]
		score := int64(v.Score)
		miner := miners[id]
		miner.HR += share

		if miner.LastBeat < score {
			miner.LastBeat = score
		}
		if miner.startedAt > score || miner.startedAt == 0 {
			miner.startedAt = score
		}
		miners[id] = miner
	}

	for id, miner := range miners {
		timeOnline := now - miner.startedAt
		if timeOnline < 600 {
			timeOnline = 600
		}

		boundary := timeOnline
		if timeOnline >= window {
			boundary = window
		}
		miner.HR = miner.HR / boundary

		if miner.LastBeat < (now - window/2) {
			miner.Offline = true
		}
		totalHashrate += miner.HR
		miners[id] = miner
	}
	return totalHashrate, miners
}

func convertPaymentsResults(raw *redis.ZSliceCmd) []map[string]interface{} {
	var result []map[string]interface{}
	for _, v := range raw.Val() {
		tx := make(map[string]interface{})
		tx["timestamp"] = int64(v.Score)
		fields := strings.Split(v.Member.(string), ":")
		tx["tx"] = fields[0]

		if len(fields) < 3 {
			tx["amount"], _ = strconv.ParseInt(fields[1], 10, 64)
		} else {
			tx["address"] = fields[1]
			tx["amount"], _ = strconv.ParseInt(fields[2], 10, 64)
		}
		result = append(result, tx)
	}
	return result
}
