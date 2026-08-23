package main

// gui_clients.go: the Session > Clients... dialog. Lists the MCP clients
// currently connected to aishwnd's Unix socket (fetched over the private
// wire link, not baked into a DLGTEMPLATE the way Settings' Environment
// tab's list is a runtime SysListView32 -- unlike that list, this one
// needs a REAL per-row button, which SysListView32 has no built-in support
// for at all, so a fixed-size DLGTEMPLATE with maxClientRows worth of
// label+button pairs, shown/hidden by row count exactly like Settings'
// General/Connection pages already do, is both simpler and sufficient:
// realistically only a handful of clients are ever connected at once.
//
// Fetching the list and disconnecting a client are both synchronous wire
// round trips on the GUI thread, mirroring menuRename's existing pattern
// (realmenu.go) -- consistent with this codebase's established shape for
// menu-originated requests, even though it means the dialog doesn't paint
// until the fetch resolves (or times out).

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"ai-ssh/internal/aishwinwire"
)

// maxClientRows caps how many connected clients the dialog can show at
// once without needing a scrollable control -- generous for the expected
// case (the aish proxy, maybe a debug CLI) while keeping the dialog a
// fixed, simple size.
const maxClientRows = 8

const (
	idClientsRefreshBtn   = 501
	idClientsNoneLabel    = 502
	idClientsRowLabelBase = 510 // 510..517
	idClientsRowBtnBase   = 520 // 520..527
)

var clientsDialogProcPtr = syscall.NewCallback(clientsDialogProc)

// currentClients is the last fetched snapshot, indexed the same as the
// row controls -- valid only while the Clients dialog is open (one at a
// time, like the other custom dialogs in this package).
var currentClients []aishwinwire.ClientData

// ShowClientsDialog displays the modal Clients window. Must be called from
// the GUI's own thread (a menu click handler, which mainWndProc already
// runs there).
func ShowClientsDialog() {
	tmpl := buildClientsDialogTemplate("Connected Clients")
	inst := getModuleHandle()
	procDialogBoxIndirectParamW.Call(
		uintptr(inst),
		uintptr(unsafe.Pointer(&tmpl[0])),
		uintptr(hwndMain),
		clientsDialogProcPtr,
		0,
	)
}

func clientsDialogProc(hwndDlg syscall.Handle, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmInitDialog:
		procSetForegroundWin.Call(uintptr(hwndDlg))
		onClientsDialogOpen(hwndDlg)
		refreshClientsDialog(hwndDlg)
		return 1
	case wmCommand:
		id := uint16(wParam & 0xFFFF)
		if uint16(wParam>>16) != bnClicked {
			return 0
		}
		switch {
		case id == idCancelBtn:
			procEndDialog.Call(uintptr(hwndDlg), 0)
			onClientsDialogClose()
			return 1
		case id == idClientsRefreshBtn:
			refreshClientsDialog(hwndDlg)
			return 1
		case id >= idClientsRowBtnBase && int(id) < idClientsRowBtnBase+maxClientRows:
			disconnectClientRow(hwndDlg, int(id)-idClientsRowBtnBase)
			return 1
		}
	case wmClose:
		procEndDialog.Call(uintptr(hwndDlg), 0)
		onClientsDialogClose()
		return 1
	}
	return 0
}

// refreshClientsDialog fetches the current client list and updates every
// row's visibility/text to match -- called on open and by the Refresh
// button, and after a disconnect so the list reflects it immediately.
func refreshClientsDialog(hwndDlg syscall.Handle) {
	clients, err := fetchClients()
	if err != nil {
		AppendLog("aishwin: " + err.Error())
		clients = nil
	}
	currentClients = clients

	noneHwnd, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), idClientsNoneLabel)
	if len(clients) == 0 {
		procShowWindow.Call(noneHwnd, swShow)
	} else {
		procShowWindow.Call(noneHwnd, swHide)
	}

	for i := 0; i < maxClientRows; i++ {
		labelHwnd, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), uintptr(idClientsRowLabelBase+i))
		btnHwnd, _, _ := procGetDlgItem.Call(uintptr(hwndDlg), uintptr(idClientsRowBtnBase+i))
		if i < len(clients) {
			procSetWindowTextW.Call(labelHwnd, uintptr(unsafe.Pointer(utf16ptr(formatClientLine(clients[i])))))
			procShowWindow.Call(labelHwnd, swShow)
			procShowWindow.Call(btnHwnd, swShow)
		} else {
			procShowWindow.Call(labelHwnd, swHide)
			procShowWindow.Call(btnHwnd, swHide)
		}
	}
}

// disconnectClientRow disconnects the client currently shown at row idx and
// refreshes the dialog to reflect the result.
func disconnectClientRow(hwndDlg syscall.Handle, idx int) {
	if idx < 0 || idx >= len(currentClients) {
		return
	}
	client := currentClients[idx]
	name := client.Name
	if name == "" {
		name = "an MCP client"
	}
	if err := sendDisconnectClient(client.ID); err != nil {
		AppendLog("aishwin: " + err.Error())
		return
	}
	AppendLog(fmt.Sprintf("aishwin: disconnected %s", name))
	refreshClientsDialog(hwndDlg)
}

// formatClientLine renders one client's row text, condensed to a single
// line (this dialog gives each client one label control, not several)
// unlike internal/mcpserver/clients.go's ClientLines, which can afford
// multiple lines per client for the Ctrl-] console menu.
func formatClientLine(c aishwinwire.ClientData) string {
	name := c.Name
	if name == "" {
		name = "an MCP client"
	}
	line := name
	if c.Version != "" {
		line += " " + c.Version
	}
	line += " — " + c.State
	if c.SinceUnix > 0 {
		since := time.Unix(c.SinceUnix, 0)
		line += ", connected " + shortDuration(time.Since(since)) + " ago"
	}
	if c.Description != "" && c.Description != c.Name {
		line += fmt.Sprintf(" (declared: %s)", c.Description)
	}
	return line
}

// shortDuration renders a connection age compactly: seconds under a
// minute, then minutes, then hours. Duplicated from
// internal/mcpserver/clients.go's function of the same name rather than
// imported -- this binary must not import internal/mcpserver (see
// main.go's file-level comment).
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	default:
		return strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
	}
}

// fetchClients asks aishwnd for the MCP clients currently connected to its
// Unix socket, mirroring menuRename's (realmenu.go) request/await/timeout
// shape for a menu-originated wire request.
func fetchClients() ([]aishwinwire.ClientData, error) {
	snap := rt.snapshot()
	if !snap.connected || snap.wire == nil {
		return nil, fmt.Errorf("not connected to the linux half")
	}
	data, err := json.Marshal(aishwinwire.ListClientsData{})
	if err != nil {
		return nil, err
	}
	id := randHex(8)
	ch := snap.wire.Await(id)
	defer snap.wire.CancelAwait(id)
	if err := snap.wire.Send(aishwinwire.Frame{Type: "list_clients", ID: id, Data: data}); err != nil {
		return nil, fmt.Errorf("listing clients: %w", err)
	}
	select {
	case f := <-ch:
		var res aishwinwire.ListClientsResultData
		if err := json.Unmarshal(f.Data, &res); err != nil {
			return nil, fmt.Errorf("listing clients: malformed response")
		}
		return res.Clients, nil
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("listing clients: timed out")
	}
}

// sendDisconnectClient asks aishwnd to close one specific client
// connection (identified by the ID a prior fetchClients reported).
func sendDisconnectClient(id string) error {
	snap := rt.snapshot()
	if !snap.connected || snap.wire == nil {
		return fmt.Errorf("not connected to the linux half")
	}
	data, err := json.Marshal(aishwinwire.DisconnectClientData{ID: id})
	if err != nil {
		return err
	}
	reqID := randHex(8)
	ch := snap.wire.Await(reqID)
	defer snap.wire.CancelAwait(reqID)
	if err := snap.wire.Send(aishwinwire.Frame{Type: "disconnect_client", ID: reqID, Data: data}); err != nil {
		return fmt.Errorf("disconnecting client: %w", err)
	}
	select {
	case f := <-ch:
		var res aishwinwire.DisconnectClientResultData
		if err := json.Unmarshal(f.Data, &res); err != nil {
			return fmt.Errorf("disconnecting client: malformed response")
		}
		if res.Error != "" {
			return fmt.Errorf("disconnecting client: %s", res.Error)
		}
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("disconnecting client: timed out")
	}
}

// buildClientsDialogTemplate assembles the whole Clients window:
// maxClientRows worth of (label, Disconnect button) pairs plus a "no
// clients" label, all created hidden (refreshClientsDialog corrects this
// in wmInitDialog before the dialog ever paints -- WM_INITDIALOG runs
// before the dialog is shown), then Refresh/Close at a fixed position
// below all of them.
func buildClientsDialogTemplate(title string) []byte {
	var buf bytes.Buffer
	w := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	align4 := func() {
		for buf.Len()%4 != 0 {
			buf.WriteByte(0)
		}
	}
	writeStr := func(s string) {
		u, _ := syscall.UTF16FromString(s)
		for _, c := range u {
			w(c)
		}
	}
	item := func(style uint32, x, y, cx, cy int16, id uint16, class uint16, text string) {
		align4()
		w(style)
		w(uint32(0))
		w(x)
		w(y)
		w(cx)
		w(cy)
		w(id)
		w(uint16(0xFFFF))
		w(class)
		writeStr(text)
		w(uint16(0))
	}

	const dialogWidth = int16(340)
	const rowTop0 = int16(15)
	const rowHeight = int16(26)
	const labelWidth = int16(230)
	const btnWidth = int16(80)
	const btnHeight = int16(20)
	bottomY := rowTop0 + int16(maxClientRows)*rowHeight + 10
	dialogHeight := bottomY + 20 + 15

	itemCount := uint16(maxClientRows*2 + 1 /* none-label */ + 2 /* refresh+close */)

	style := uint32(dsSetFont | dsModalFrame | dsCenter | wsPopup | wsCaption | wsSysMenu)
	w(style)
	w(uint32(0))
	w(itemCount)
	w(int16(0))
	w(int16(0))
	w(dialogWidth)
	w(dialogHeight)
	w(uint16(0))
	w(uint16(0))
	writeStr(title)
	w(uint16(8))
	writeStr("MS Shell Dlg")

	item(wsChild, 10, rowTop0, dialogWidth-20, 20, idClientsNoneLabel, 0x0082, "No MCP clients are connected.")

	y := rowTop0
	for i := 0; i < maxClientRows; i++ {
		item(wsChild, 10, y, labelWidth, rowHeight-4, uint16(idClientsRowLabelBase+i), 0x0082, "")
		item(wsChild|wsTabStop|bsPushButton, 10+labelWidth+10, y, btnWidth, btnHeight, uint16(idClientsRowBtnBase+i), 0x0080, "Disconnect")
		y += rowHeight
	}

	item(wsChild|wsVisible|wsTabStop|bsPushButton, 10, bottomY, 90, 20, idClientsRefreshBtn, 0x0080, "Refresh")
	item(wsChild|wsVisible|wsTabStop|bsDefPushButton, dialogWidth-100, bottomY, 90, 20, idCancelBtn, 0x0080, "Close")

	return buf.Bytes()
}

// onClientsDialogOpen/onClientsDialogClose are hooks devctl.go
// (aishwindev build tag only) replaces to track the currently-open
// Clients dialog's HWND for automated testing; a no-op in an ordinary
// build.
var onClientsDialogOpen = func(hwnd syscall.Handle) {}
var onClientsDialogClose = func() {}
