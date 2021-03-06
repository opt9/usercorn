package trace

import (
	"encoding/binary"

	"github.com/lunixbochs/usercorn/go/models"
	"github.com/lunixbochs/usercorn/go/models/cpu"
	"github.com/lunixbochs/usercorn/go/models/debug"
)

type Replay struct {
	Arch   *models.Arch
	OS     *models.OS
	Mem    *cpu.Mem
	Regs   map[int]uint64
	SpRegs map[int][]byte
	PC, SP uint64

	Callstack models.Callstack
	Debug     *debug.Debug
	Inscount  uint64
	// pending is an OpStep representing the last unflushed instruction. Cleared by Flush().
	pending   *OpStep
	effects   []models.Op
	callbacks []func(models.Op, []models.Op)
}

func NewReplay(arch *models.Arch, os *models.OS, order binary.ByteOrder, dbg *debug.Debug) *Replay {
	return &Replay{
		Arch:   arch,
		OS:     os,
		Mem:    cpu.NewMem(uint(arch.Bits), order),
		Regs:   make(map[int]uint64),
		SpRegs: make(map[int][]byte),
		Debug:  dbg,
	}
}

func (r *Replay) Listen(cb func(models.Op, []models.Op)) {
	r.callbacks = append(r.callbacks, cb)
}

// update() applies state change(s) from op to the UI's internal state
func (r *Replay) update(op models.Op) {
	switch o := op.(type) {
	case *OpJmp: // code
		r.PC = o.Addr
	case *OpStep:
		r.PC += uint64(o.Size)

	case *OpReg: // register
		if int(o.Num) == r.Arch.SP {
			r.SP = o.Val
			r.Callstack.Update(r.PC, r.SP)
		}
		r.Regs[int(o.Num)] = o.Val
	case *OpSpReg:
		r.SpRegs[int(o.Num)] = o.Val

	case *OpMemMap: // memory
		// TODO: can this be changed to not need direct access to Sim?
		page := r.Mem.Sim.Map(o.Addr, uint64(o.Size), int(o.Prot), true)
		page.Desc = o.Desc
		if o.File != "" {
			page.File = &cpu.FileDesc{Name: o.File, Off: o.Off, Len: o.Len}
		}
	case *OpMemUnmap:
		r.Mem.MemUnmap(o.Addr, uint64(o.Size))
	case *OpMemProt:
		r.Mem.MemProt(o.Addr, uint64(o.Size), int(o.Prot))
	case *OpMemWrite:
		r.Mem.MemWrite(o.Addr, o.Data)

	case *OpSyscall:
		for _, v := range o.Ops {
			r.update(v)
		}
	}
}

// Feed() is the entry point handling Op structs.
// It calls update() and combines side-effects with instructions
func (r *Replay) Feed(op models.Op) {
	var ops []models.Op
	switch o := op.(type) {
	case *OpFrame:
		ops = o.Ops
	default:
		ops = []models.Op{op}

	case *OpKeyframe:
		// we need to flush here, because the keyframe can change state we need to emit
		r.Flush()
		// We only need the first keyframe for simple display (until we're doing rewind/ff)
		// but it probably doesn't hurt too much for now to always process keyframes... just don't emit them
		for _, v := range o.Ops {
			r.update(v)
		}
		return
	}

	for _, op := range ops {
		// batch everything until we hit an OpJmp or OpStep
		// at that point, flush the last OpStep
		switch o := op.(type) {
		case *OpJmp:
			// fixes a bug where single-stepping misattributes registers
			if o.Addr != r.PC {
				r.Flush()
			}
			r.Emit(o, nil)
			r.update(o)
		case *OpStep:
			r.Flush()
			r.pending = o
		case *OpSyscall:
			r.Flush()
			r.update(o)
			r.Emit(o, o.Ops)
		default:
			// queue everything else as side-effects
			r.effects = append(r.effects, op)
		}
	}
	// flush at end of frame too, so repl isn't an instruction behind when single stepping
	r.Flush()
}

func (r *Replay) Emit(op models.Op, effects []models.Op) {
	for _, cb := range r.callbacks {
		cb(op, effects)
	}
}

func (r *Replay) Flush() {
	if r.pending != nil {
		r.Emit(r.pending, r.effects)
		r.Inscount += 1
		r.update(r.pending)
		for _, op := range r.effects {
			r.update(op)
		}
		r.effects = r.effects[:0]
		r.pending = nil
	}
}

func (r *Replay) Symbolicate(addr uint64, includeSource bool) (*models.Symbol, string) {
	return r.Debug.Symbolicate(addr, r.Mem.Maps(), includeSource)
}
