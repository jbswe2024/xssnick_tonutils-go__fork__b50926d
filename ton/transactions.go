package ton

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"time"
)

func init() {
	tl.Register(GetOneTransaction{}, "liteServer.getOneTransaction id:tonNode.blockIdExt account:liteServer.accountId lt:long = liteServer.TransactionInfo")
	tl.Register(GetTransactions{}, "liteServer.getTransactions count:# account:liteServer.accountId lt:long hash:int256 = liteServer.TransactionList")
	tl.Register(TransactionList{}, "liteServer.transactionList ids:(vector tonNode.blockIdExt) transactions:bytes = liteServer.TransactionList")
	tl.Register(TransactionInfo{}, "liteServer.transactionInfo id:tonNode.blockIdExt proof:bytes transaction:bytes = liteServer.TransactionInfo")
}

type TransactionInfo struct {
	ID          *BlockIDExt `tl:"struct"`
	Proof       []byte      `tl:"bytes"`
	Transaction []byte      `tl:"bytes"`
}

type TransactionList struct {
	IDs          []*BlockIDExt `tl:"vector struct"`
	Transactions []byte        `tl:"bytes"`
}

type GetOneTransaction struct {
	ID    *BlockIDExt `tl:"struct"`
	AccID *AccountID  `tl:"struct"`
	LT    int64       `tl:"long"`
}

type GetTransactions struct {
	Limit  int32      `tl:"int"`
	AccID  *AccountID `tl:"struct"`
	LT     int64      `tl:"long"`
	TxHash []byte     `tl:"int256"`
}

// ListTransactions - returns list of transactions before (including) passed lt and hash, the oldest one is first in result slice
// Transactions will be verified to match final tx hash, which should be taken from proved account state, then it is safe.
func (c *APIClient) ListTransactions(ctx context.Context, addr *address.Address, limit uint32, lt uint64, txHash []byte) ([]*tlb.Transaction, error) {
	var resp tl.Serializable
	err := c.client.QueryLiteserver(ctx, GetTransactions{
		Limit: int32(limit),
		AccID: &AccountID{
			Workchain: addr.Workchain(),
			ID:        addr.Data(),
		},
		LT:     int64(lt),
		TxHash: txHash,
	}, &resp)
	if err != nil {
		return nil, err
	}

	switch t := resp.(type) {
	case TransactionList:
		txList, err := cell.FromBOCMultiRoot(t.Transactions)
		if err != nil {
			return nil, fmt.Errorf("failed to parse cell from transaction bytes: %w", err)
		}

		res := make([]*tlb.Transaction, len(txList))

		for i := len(txList) - 1; i >= 0; i-- {
			loader := txList[i].BeginParse()

			var tx tlb.Transaction
			err = tlb.LoadFromCell(&tx, loader)
			if err != nil {
				return nil, fmt.Errorf("failed to load transaction from cell: %w", err)
			}
			tx.Hash = txList[i].Hash()

			if !bytes.Equal(txHash, tx.Hash) {
				return nil, fmt.Errorf("incorrect transaction hash, not matches prev tx hash")
			}
			txHash = tx.PrevTxHash
			res[i] = &tx
		}
		return res, nil
	case LSError:
		if t.Code == 0 {
			return nil, ErrMessageNotAccepted
		}
		return nil, t
	}

	return nil, errors.New("unknown response type")
}

func (c *APIClient) GetTransaction(ctx context.Context, block *BlockIDExt, addr *address.Address, lt uint64) (*tlb.Transaction, error) {
	var resp tl.Serializable
	err := c.client.QueryLiteserver(ctx, GetOneTransaction{
		ID: block,
		AccID: &AccountID{
			Workchain: addr.Workchain(),
			ID:        addr.Data(),
		},
		LT: int64(lt),
	}, &resp)
	if err != nil {
		return nil, err
	}

	switch t := resp.(type) {
	case TransactionInfo:
		if !t.ID.Equals(block) {
			return nil, fmt.Errorf("incorrect block in response")
		}

		txCell, err := cell.FromBOC(t.Transaction)
		if err != nil {
			return nil, fmt.Errorf("failed to parse cell from transaction bytes: %w", err)
		}

		var tx tlb.Transaction
		err = tlb.LoadFromCell(&tx, txCell.BeginParse())
		if err != nil {
			return nil, fmt.Errorf("failed to load transaction from cell: %w", err)
		}
		tx.Hash = txCell.Hash()

		if c.proofCheckPolicy != ProofCheckPolicyUnsafe {
			txProof, err := cell.FromBOC(t.Proof)
			if err != nil {
				return nil, fmt.Errorf("failed to parse proof: %w", err)
			}

			blockProof, err := CheckBlockProof(txProof, block.RootHash)
			if err != nil {
				return nil, fmt.Errorf("failed to check proof: %w", err)
			}

			if blockProof.Extra == nil || blockProof.Extra.ShardAccountBlocks == nil {
				return nil, fmt.Errorf("block proof without shard accounts")
			}

			var shardAccounts tlb.ShardAccountBlocks
			err = tlb.LoadFromCellAsProof(&shardAccounts, blockProof.Extra.ShardAccountBlocks.BeginParse())
			if err != nil {
				return nil, fmt.Errorf("failed to load shard accounts from proof: %w", err)
			}

			if err = CheckTransactionProof(tx.Hash, tx.LT, tx.AccountAddr, &shardAccounts); err != nil {
				return nil, fmt.Errorf("incorrect tx proof: %w", err)
			}
		}

		return &tx, nil
	case LSError:
		if t.Code == 0 {
			return nil, ErrMessageNotAccepted
		}
		return nil, t
	}
	return nil, errUnexpectedResponse(resp)
}

func (c *APIClient) SubscribeOnTransactions(workerCtx context.Context, addr *address.Address, lastProcessedLT uint64, channel chan<- *tlb.Transaction) {
	defer func() {
		close(channel)
	}()

	wait := 0 * time.Second
	for {
		select {
		case <-workerCtx.Done():
			return
		case <-time.After(wait):
		}
		wait = 3 * time.Second

		ctx, cancel := context.WithTimeout(workerCtx, 10*time.Second)
		master, err := c.CurrentMasterchainInfo(ctx)
		cancel()
		if err != nil {
			continue
		}

		ctx, cancel = context.WithTimeout(workerCtx, 10*time.Second)
		acc, err := c.GetAccount(ctx, master, addr)
		cancel()
		if err != nil {
			continue
		}
		if !acc.IsActive || acc.LastTxLT == 0 {
			// no transactions
			continue
		}

		if lastProcessedLT == acc.LastTxLT {
			// already processed all
			continue
		}

		var transactions []*tlb.Transaction
		lastHash, lastLT := acc.LastTxHash, acc.LastTxLT

		waitList := 0 * time.Second
	list:
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-time.After(waitList):
			}

			// ctx = workerCtx
			ctx, cancel = context.WithTimeout(workerCtx, 10*time.Second)
			res, err := c.ListTransactions(ctx, addr, 10, lastLT, lastHash)
			cancel()
			if err != nil {
				if lsErr, ok := err.(LSError); ok && lsErr.Code == -400 {
					// lt not in db error
					return
				}
				waitList = 3 * time.Second
				continue
			}

			if len(res) == 0 {
				break
			}

			// reverse slice
			for i, j := 0, len(res)-1; i < j; i, j = i+1, j-1 {
				res[i], res[j] = res[j], res[i]
			}

			for i, tx := range res {
				if tx.LT <= lastProcessedLT {
					transactions = append(transactions, res[:i]...)
					break list
				}
			}

			lastLT, lastHash = res[len(res)-1].PrevTxLT, res[len(res)-1].PrevTxHash
			transactions = append(transactions, res...)
			waitList = 0 * time.Second
		}
		lastProcessedLT = transactions[0].LT // mark last transaction as known to not trigger twice

		// reverse slice to send in correct time order (from old to new)
		for i, j := 0, len(transactions)-1; i < j; i, j = i+1, j-1 {
			transactions[i], transactions[j] = transactions[j], transactions[i]
		}

		for _, tx := range transactions {
			channel <- tx
		}

		if len(transactions) > 0 {
			wait = 0 * time.Second
		}
	}
}
