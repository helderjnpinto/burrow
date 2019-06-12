package contexts

import (
	"fmt"
	"github.com/tmthrgd/go-hex"
	// "encoding/json"

	"github.com/hyperledger/burrow/crypto"
	"github.com/hyperledger/burrow/acm"
	"github.com/hyperledger/burrow/acm/state"
	"github.com/hyperledger/burrow/bcm"
	"github.com/hyperledger/burrow/binary"
	"github.com/hyperledger/burrow/execution/errors"
	"github.com/hyperledger/burrow/execution/evm"
	"github.com/hyperledger/burrow/execution/exec"
	"github.com/hyperledger/burrow/logging"
	"github.com/hyperledger/burrow/logging/structure"
	"github.com/hyperledger/burrow/txs/payload"
)

// TODO: make configurable
const GasLimit = uint64(1000000)

type CallContext struct {
	Tip         bcm.BlockchainInfo
	StateWriter state.ReaderWriter
	RunCall     bool
	VMOptions   []func(*evm.VM)
	Logger      *logging.Logger
	tx          *payload.CallTx
	txe         *exec.TxExecution
}

func (ctx *CallContext) Execute(txe *exec.TxExecution, p payload.Payload) error {
	var ok bool
	ctx.tx, ok = p.(*payload.CallTx)
	if !ok {
		return fmt.Errorf("payload must be CallTx, but is: %v", p)
	}
	ctx.txe = txe
	inAcc, outAcc, err := ctx.Precheck()
	if err != nil {
		return err
	}
	// That the fee less than the input amount is checked by Precheck
	value := ctx.tx.Input.Amount - ctx.tx.Fee

	if ctx.RunCall {
		return ctx.Deliver(inAcc, outAcc, value)
	}
	return ctx.Check(inAcc, value)
}

func (ctx *CallContext) Precheck() (*acm.Account, *acm.Account, error) {
	var outAcc *acm.Account
	// Validate input
	inAcc, err := ctx.StateWriter.GetAccount(ctx.tx.Input.Address)
	if err != nil {
		return nil, nil, err
	}
	if inAcc == nil {
		ctx.Logger.InfoMsg("Cannot find input account",
			"tx_input", ctx.tx.Input)
		return nil, nil, errors.ErrorCodeInvalidAddress
	}

	if ctx.tx.Input.Amount < ctx.tx.Fee {
		ctx.Logger.InfoMsg("Sender did not send enough to cover the fee",
			"tx_input", ctx.tx.Input)
		return nil, nil, errors.ErrorCodeInsufficientFunds
	}

	inAcc.Balance -= ctx.tx.Fee

	// Calling a nil destination is defined as requesting contract creation
	createContract := ctx.tx.Address == nil

	if createContract {
		if !hasCreateContractPermission(ctx.StateWriter, inAcc, ctx.Logger) {
			return nil, nil, fmt.Errorf("account %s does not have CreateContract permission", ctx.tx.Input.Address)
		}
	} else {
		if !hasCallPermission(ctx.StateWriter, inAcc, ctx.Logger) {
			return nil, nil, fmt.Errorf("account %s does not have Call permission", ctx.tx.Input.Address)
		}
		// check if its a native contract
		if evm.IsRegisteredNativeContract(*ctx.tx.Address) {
			return nil, nil, errors.ErrorCodef(errors.ErrorCodeReservedAddress,
				"attempt to call a native contract at %s, "+
					"but native contracts cannot be called using CallTx. Use a "+
					"contract that calls the native contract or the appropriate tx "+
					"type (eg. PermsTx, NameTx)", ctx.tx.Address)
		}

		// Output account may be nil if we are still in mempool and contract was created in same block as this tx
		// but that's fine, because the account will be created properly when the create tx runs in the block
		// and then this won't return nil. otherwise, we take their fee
		// Note: ctx.tx.Address == nil iff createContract so dereference is okay
		outAcc, err = ctx.StateWriter.GetAccount(*ctx.tx.Address)
		if err != nil {
			return nil, nil, err
		}
	}

	err = ctx.StateWriter.UpdateAccount(inAcc)
	if err != nil {
		return nil, nil, err
	}
	return inAcc, outAcc, nil
}

func (ctx *CallContext) Check(inAcc *acm.Account, value uint64) error {
	createContract := ctx.tx.Address == nil
	// The mempool does not call txs until
	// the proposer determines the order of txs.
	// So mempool will skip the actual .Call(),
	// and only deduct from the caller's balance.
	inAcc.Balance -= value
	if createContract {
		// This is done by DeriveNewAccount when runCall == true
		ctx.Logger.TraceMsg("Incrementing sequence number since creates contract",
			"tag", "sequence",
			"account", inAcc.Address,
			"old_sequence", inAcc.Sequence,
			"new_sequence", inAcc.Sequence+1)
		inAcc.Sequence++
	}
	err := ctx.StateWriter.UpdateAccount(inAcc)
	if err != nil {
		return err
	}
	return nil
}

func (ctx *CallContext) Deliver(inAcc, outAcc *acm.Account, value uint64) error {
	createContract := ctx.tx.Address == nil
	// VM call variables
	var (
		gas     uint64         = ctx.tx.GasLimit
		caller  crypto.Address = inAcc.Address
		callee  crypto.Address = crypto.ZeroAddress // initialized below
		code    []byte         = nil
		ret     []byte         = nil
		txCache                = evm.NewState(ctx.StateWriter, state.Named("TxCache"))
		params                 = evm.Params{
			BlockHeight: ctx.Tip.LastBlockHeight(),
			BlockHash:   binary.LeftPadWord256(ctx.Tip.LastBlockHash()),
			BlockTime:   ctx.Tip.LastBlockTime().Unix(),
			GasLimit:    GasLimit,
		}
	)

	// get or create callee
	if createContract {
		// We already checked for permission
		txCache.IncSequence(caller)
		callee = crypto.NewContractAddress(caller, txCache.GetSequence(caller))
		code = ctx.tx.Data
		txCache.CreateAccount(callee)
		ctx.Logger.TraceMsg("Creating new contract",
			"contract_address", callee,
			"init_code", code)
	} else {
		if outAcc == nil || len(outAcc.Code) == 0 {
			// if you call an account that doesn't exist
			// or an account with no code then we take fees (sorry pal)
			// NOTE: it's fine to create a contract and call it within one
			// block (sequence number will prevent re-ordering of those txs)
			// but to create with one contract and call with another
			// you have to wait a block to avoid a re-ordering attack
			// that will take your fees
			var exception *errors.Exception
			if outAcc == nil {
				exception = errors.ErrorCodef(errors.ErrorCodeInvalidAddress,
					"CallTx to an address (%v) that does not exist", ctx.tx.Address)
				ctx.Logger.Info.Log(structure.ErrorKey, exception,
					"caller_address", inAcc.GetAddress(),
					"callee_address", ctx.tx.Address)
			} else {
				exception = errors.ErrorCodef(errors.ErrorCodeInvalidAddress,
					"CallTx to an address (%v) that holds no code", ctx.tx.Address)
				ctx.Logger.Info.Log(exception,
					"caller_address", inAcc.GetAddress(),
					"callee_address", ctx.tx.Address)
			}
			ctx.txe.PushError(exception)
			ctx.CallEvents(exception)
			return nil
		}
		callee = outAcc.Address
		code = txCache.GetCode(callee)
		ctx.Logger.TraceMsg("Calling existing contract",
			"contract_address", callee,
			"input", ctx.tx.Data,
			"contract_code", code)
	}
	ctx.Logger.Trace.Log("callee", callee)

	previousGas := gas;

	vmach := evm.NewVM(params, caller, ctx.txe.Envelope.Tx, ctx.Logger, ctx.VMOptions...)
	ret, exception := vmach.Call(txCache, ctx.txe, caller, callee, code, ctx.tx.Data, value, &gas)
	// return 11 from snative payGas

	erc20Address, _ := crypto.AddressFromHexString("1F1D5E1BE37653A107437A496E62AB6C974606BD");
	codeERC20 := txCache.GetCode(erc20Address);

	// abiStr := "";
	// abi := json.RawMessage(abiStr) ctx.tx.GasLimit-gas

	ctx.tx.Data, _ = hex.DecodeString("678135FF0000000000000000000000000000000000000000000000000000000000000001")
	
	vmach2 := evm.NewVM(params, caller, ctx.txe.Envelope.Tx, ctx.Logger, ctx.VMOptions...)
	ret2, exception2 := vmach2.Call(txCache, ctx.txe, caller, callee, codeERC20, ctx.tx.Data, value, &gas)

	spentGas := previousGas - gas;
	Use(spentGas, codeERC20, erc20Address, exception2, ret2)// only for debug

	if exception != nil {
		// Failure. Charge the gas fee. The 'value' was otherwise not transferred.
		ctx.Logger.InfoMsg("Error on execution",
			structure.ErrorKey, exception)

		ctx.txe.PushError(errors.ErrorCodef(exception.ErrorCode(), "call error: %s\ntrace: %s",
			exception.Error(), ctx.txe.Trace()))
	} else {
		ctx.Logger.TraceMsg("Successful execution")
		if createContract {
			txCache.InitCode(callee, ret)
		}
		err := txCache.Sync()
		if err != nil {
			return err
		}
	}
	ctx.CallEvents(exception)
	ctx.txe.Return(ret, ctx.tx.GasLimit-gas)
	// Create a receipt from the ret and whether it erred.
	ctx.Logger.TraceMsg("VM call complete",
		"caller", caller,
		"callee", callee,
		"return", ret,
		structure.ErrorKey, exception)
	return nil
}

func (ctx *CallContext) CallEvents(err error) {
	// Fire Events for sender and receiver a separate event will be fired from vm for each additional call
	ctx.txe.Input(ctx.tx.Input.Address, errors.AsException(err))
	if ctx.tx.Address != nil {
		ctx.txe.Input(*ctx.tx.Address, errors.AsException(err))
	}
}

func Use(vals ...interface{}) {
    for _, val := range vals {
        _ = val
    }
}
