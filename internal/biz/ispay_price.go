package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strings"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	swapConfigURL          = "https://api.swap.ispay.vip/api/config"
	swapDefaultRPC         = "https://rpc.ispay.vip"
	swapDefaultFactory     = "0xe74C3DB4db726Df8494B25C688b8465cEc1bd069"
	swapDefaultIspayToken  = "0xa125ffdd613be2b508b2db651f06161699999999"
	swapDefaultUsdtToken   = "0x4401e4a1a39c709a66d6195f1e3961bd5949bde7"
	swapDefaultIspayDec    = 18
	swapDefaultUsdtDec     = 6
	swapIspayChainConfigID = "1"
)

type swapConfigResp struct {
	Code int `json:"code"`
	Data struct {
		Swap struct {
			FactoryAddress string `json:"factory_address"`
		} `json:"swap"`
		Chain []struct {
			ID      int    `json:"id"`
			ChainID string `json:"chain_id"`
			RpcURL  string `json:"rpc_url"`
			Symbol  string `json:"symbol"`
		} `json:"chain"`
		SwapTokens []struct {
			Address    string `json:"address"`
			Symbol     string `json:"symbol"`
			Decimals   int    `json:"decimals"`
			ChainID    string `json:"chain_id"`
			SwapStatus int    `json:"swap_status"`
		} `json:"swap_tokens"`
	} `json:"data"`
}

type swapPriceSource struct {
	RPC      string
	Factory  common.Address
	Ispay    common.Address
	Usdt     common.Address
	IspayDec int
	UsdtDec  int
}

// GetIspayPrice returns USDT per 1 ISPAY using the same Uniswap V2
// getAmountsOut formula as swap.ispay.vip 闪兑.
func GetIspayPrice() (float64, error) {
	src, err := loadSwapPriceSource()
	if err != nil {
		src = defaultSwapPriceSource()
	}
	price, err := quoteIspayUsdt(src)
	if err == nil && price > 0 {
		return price, nil
	}
	if src.RPC != swapDefaultRPC {
		fallback := defaultSwapPriceSource()
		return quoteIspayUsdt(fallback)
	}
	if err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("ISPAY/USDT swap price is zero")
}

func defaultSwapPriceSource() swapPriceSource {
	return swapPriceSource{
		RPC:      swapDefaultRPC,
		Factory:  common.HexToAddress(swapDefaultFactory),
		Ispay:    common.HexToAddress(swapDefaultIspayToken),
		Usdt:     common.HexToAddress(swapDefaultUsdtToken),
		IspayDec: swapDefaultIspayDec,
		UsdtDec:  swapDefaultUsdtDec,
	}
}

func loadSwapPriceSource() (swapPriceSource, error) {
	src := defaultSwapPriceSource()
	client := &http.Client{Timeout: 8 * time.Second}
	req, err := http.NewRequest(http.MethodGet, swapConfigURL, nil)
	if err != nil {
		return src, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		return src, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return src, fmt.Errorf("swap config status %d", resp.StatusCode)
	}
	var cfg swapConfigResp
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return src, err
	}
	if cfg.Data.Swap.FactoryAddress != "" {
		src.Factory = common.HexToAddress(cfg.Data.Swap.FactoryAddress)
	}
	for _, ch := range cfg.Data.Chain {
		if strings.EqualFold(ch.Symbol, "ISPAY") && ch.RpcURL != "" {
			src.RPC = ch.RpcURL
			break
		}
	}
	var ispayAddr, usdtAddr string
	ispayDec, usdtDec := 0, 0
	for _, tok := range cfg.Data.SwapTokens {
		if tok.SwapStatus != 1 || tok.ChainID != swapIspayChainConfigID {
			continue
		}
		if strings.EqualFold(tok.Symbol, "USDT") {
			usdtAddr, usdtDec = tok.Address, tok.Decimals
		}
		if strings.EqualFold(tok.Symbol, "ISPAY") {
			ispayAddr, ispayDec = tok.Address, tok.Decimals
		}
	}
	if ispayAddr == "" || usdtAddr == "" {
		return src, fmt.Errorf("swap tokens not found")
	}
	src.Ispay = common.HexToAddress(ispayAddr)
	src.Usdt = common.HexToAddress(usdtAddr)
	if ispayDec > 0 {
		src.IspayDec = ispayDec
	}
	if usdtDec > 0 {
		src.UsdtDec = usdtDec
	}
	return src, nil
}

func quoteIspayUsdt(src swapPriceSource) (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := ethclient.DialContext(ctx, src.RPC)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	pair, err := swapGetPair(ctx, client, src.Factory, src.Ispay, src.Usdt)
	if err != nil {
		return 0, err
	}
	if (pair == common.Address{}) {
		return 0, fmt.Errorf("ISPAY/USDT pair not found")
	}
	token0, err := swapToken0(ctx, client, pair)
	if err != nil {
		return 0, err
	}
	r0, r1, err := swapGetReserves(ctx, client, pair)
	if err != nil {
		return 0, err
	}
	var reserveIn, reserveOut *big.Int
	if strings.EqualFold(token0.Hex(), src.Ispay.Hex()) {
		reserveIn, reserveOut = r0, r1
	} else if strings.EqualFold(token0.Hex(), src.Usdt.Hex()) {
		reserveIn, reserveOut = r1, r0
	} else {
		return 0, fmt.Errorf("unexpected pair tokens")
	}
	if reserveIn.Sign() <= 0 || reserveOut.Sign() <= 0 {
		return 0, fmt.Errorf("empty ISPAY/USDT reserves")
	}
	amountIn := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(src.IspayDec)), nil)
	amountOut := uniswapV2GetAmountOut(amountIn, reserveIn, reserveOut)
	if amountOut.Sign() <= 0 {
		return 0, fmt.Errorf("ISPAY/USDT amountOut is zero")
	}
	price := new(big.Float).Quo(new(big.Float).SetInt(amountOut), new(big.Float).SetFloat64(math.Pow10(src.UsdtDec)))
	out, _ := price.Float64()
	if out <= 0 || math.IsNaN(out) || math.IsInf(out, 0) {
		return 0, fmt.Errorf("ISPAY/USDT price invalid")
	}
	return out, nil
}

func uniswapV2GetAmountOut(amountIn, reserveIn, reserveOut *big.Int) *big.Int {
	amountInWithFee := new(big.Int).Mul(amountIn, big.NewInt(997))
	numerator := new(big.Int).Mul(amountInWithFee, reserveOut)
	denominator := new(big.Int).Add(new(big.Int).Mul(reserveIn, big.NewInt(1000)), amountInWithFee)
	if denominator.Sign() == 0 {
		return big.NewInt(0)
	}
	return new(big.Int).Div(numerator, denominator)
}

func swapGetPair(ctx context.Context, client *ethclient.Client, factory, tokenA, tokenB common.Address) (common.Address, error) {
	data := append(append(common.Hex2Bytes("e6a43905"), common.LeftPadBytes(tokenA.Bytes(), 32)...), common.LeftPadBytes(tokenB.Bytes(), 32)...)
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &factory, Data: data}, nil)
	if err != nil {
		return common.Address{}, err
	}
	if len(out) < 32 {
		return common.Address{}, fmt.Errorf("getPair short result")
	}
	return common.BytesToAddress(out[len(out)-20:]), nil
}

func swapToken0(ctx context.Context, client *ethclient.Client, pair common.Address) (common.Address, error) {
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &pair, Data: common.Hex2Bytes("0dfe1681")}, nil)
	if err != nil {
		return common.Address{}, err
	}
	if len(out) < 32 {
		return common.Address{}, fmt.Errorf("token0 short result")
	}
	return common.BytesToAddress(out[len(out)-20:]), nil
}

func swapGetReserves(ctx context.Context, client *ethclient.Client, pair common.Address) (*big.Int, *big.Int, error) {
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &pair, Data: common.Hex2Bytes("0902f1ac")}, nil)
	if err != nil {
		return nil, nil, err
	}
	if len(out) < 64 {
		return nil, nil, fmt.Errorf("getReserves short result")
	}
	return new(big.Int).SetBytes(out[0:32]), new(big.Int).SetBytes(out[32:64]), nil
}
