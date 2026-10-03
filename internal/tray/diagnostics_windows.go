//go:build windows

package tray

import (
	"errors"
	"fmt"
	"time"
	utf16codec "unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

type openFileName struct {
	Size          uint32
	Owner         uintptr
	Instance      uintptr
	Filter        *uint16
	CustomFilter  *uint16
	MaxCustom     uint32
	FilterIndex   uint32
	File          *uint16
	MaxFile       uint32
	FileTitle     *uint16
	MaxFileTitle  uint32
	InitialDir    *uint16
	Title         *uint16
	Flags         uint32
	FileOffset    uint16
	FileExtension uint16
	DefaultExt    *uint16
	CustomData    uintptr
	Hook          uintptr
	Template      *uint16
	Reserved      uintptr
	ReservedWord  uint32
	FlagsEx       uint32
}

func chooseDiagnosticFile(owner uintptr) (string, error) {
	buffer := make([]uint16, 32768)
	name, _ := windows.UTF16FromString("codexfold-diagnostics-" + time.Now().Format("20060102-150405") + ".json")
	copy(buffer, name)
	filter := utf16codec.Encode([]rune("JSON 诊断文件\x00*.json\x00所有文件\x00*.*\x00\x00"))
	dialog := openFileName{Owner: owner, Filter: &filter[0], FilterIndex: 1, File: &buffer[0], MaxFile: uint32(len(buffer)),
		Title: utf16("导出 CodexFold 诊断"), DefaultExt: utf16("json"), Flags: 0x2 | 0x8 | 0x800 | 0x80000}
	dialog.Size = uint32(unsafe.Sizeof(dialog))
	common := windows.NewLazySystemDLL("comdlg32.dll")
	if ok, _, _ := common.NewProc("GetSaveFileNameW").Call(uintptr(unsafe.Pointer(&dialog))); ok == 0 {
		code, _, _ := common.NewProc("CommDlgExtendedError").Call()
		if code != 0 {
			return "", fmt.Errorf("保存窗口错误 0x%x", code)
		}
		return "", nil
	}
	return windows.UTF16ToString(buffer), nil
}

func copyDiagnosticText(owner uintptr, value string) error {
	text, err := windows.UTF16FromString(value)
	if err != nil {
		return err
	}
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	if ok, _, _ := user32.NewProc("OpenClipboard").Call(owner); ok == 0 {
		return errors.New("剪贴板正被其他程序占用，请稍后重试")
	}
	defer user32.NewProc("CloseClipboard").Call()
	memory, _, _ := kernel.NewProc("GlobalAlloc").Call(2, uintptr(len(text)*2))
	if memory == 0 {
		return errors.New("无法分配剪贴板内存")
	}
	owned := true
	defer func() {
		if owned {
			kernel.NewProc("GlobalFree").Call(memory)
		}
	}()
	address, _, _ := kernel.NewProc("GlobalLock").Call(memory)
	if address == 0 {
		return errors.New("无法写入剪贴板")
	}
	moveMemory.Call(address, uintptr(unsafe.Pointer(&text[0])), uintptr(len(text)*2))
	kernel.NewProc("GlobalUnlock").Call(memory)
	if ok, _, _ := user32.NewProc("EmptyClipboard").Call(); ok == 0 {
		return errors.New("无法清空剪贴板")
	}
	if ok, _, _ := user32.NewProc("SetClipboardData").Call(13, memory); ok == 0 { // CF_UNICODETEXT
		return errors.New("复制失败，请稍后重试")
	}
	owned = false // Windows owns the allocation after SetClipboardData.
	return nil
}

func (a *application) reply(result ActionResult) {
	data, err := marshalActionResult(result)
	if err == nil {
		a.browser.Eval("window.showOperationResult && window.showOperationResult(" + string(data) + ")")
	}
}

func (a *application) exportDiagnostics() {
	path, err := chooseDiagnosticFile(a.window)
	if err == nil && path == "" {
		return
	}
	if err == nil {
		err = ExportDiagnostics(path, a.view)
	}
	result := ActionResult{ID: "export-diagnostics", OK: err == nil, Message: "诊断已导出：" + path}
	if err != nil {
		result.Message = "导出失败：" + err.Error()
	}
	a.reply(result)
}

func (a *application) copyDiagnostics() {
	data, err := DiagnosticBytes(a.view)
	if err == nil {
		err = copyDiagnosticText(a.window, string(data))
	}
	result := ActionResult{ID: "copy-diagnostics", OK: err == nil, Message: "诊断已复制，不含会话内容。"}
	if err != nil {
		result.Message = "复制失败：" + err.Error()
	}
	a.reply(result)
}
