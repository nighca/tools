// Copyright 2022 The GoPlus Authors (goplus.org). All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall/js"

	"golang.org/x/tools/gopls/internal/lsp"
	"golang.org/x/tools/gopls/internal/lsp/cache"
	"golang.org/x/tools/gopls/internal/lsp/protocol"
	"golang.org/x/tools/internal/xcontext"
)

type WebConn struct {
	seq        int64      // must only be accessed using atomic operations
	writeMu    sync.Mutex // protects writes to the stream
	pendingMu  sync.Mutex // protects the pending map
	pending    map[ID]chan *Response
	jsListener *js.Value
	handler    Handler
}

func (s *WebConn) fail(e error) {
	// TODO
}

func (c *WebConn) replier(req Request) Replier {
	return func(ctx context.Context, result interface{}, err error) error {
		call, ok := req.(*Call)
		if !ok {
			// request was a notify, no need to respond
			return nil
		}
		response, err := NewResponse(call.id, result, err)
		if err != nil {
			return err
		}
		err = c.write(ctx, response)
		if err != nil {
			// TODO(iancottrell): if a write fails, we really need to shut down
			// the whole stream
			return err
		}
		return nil
	}
}

func (s *WebConn) receive(data *js.Value) {
	dataBytes := []byte(data.String())
	msg, err := DecodeMessage(dataBytes)
	if err != nil {
		// The stream failed, we cannot continue.
		s.fail(err)
		return
	}
	switch msg := msg.(type) {
	case Request:
		ctx := context.TODO()
		if err := s.handler(ctx, s.replier(msg), msg); err != nil {
			// TODO
		}
	case *Response:
		// If method is not set, this should be a response, in which case we must
		// have an id to send the response back to the caller.
		s.pendingMu.Lock()
		rchan, ok := s.pending[msg.id]
		s.pendingMu.Unlock()
		if ok {
			rchan <- msg
		}
	}
}

func (s *WebConn) Run(handler Handler) {
	s.handler = handler
	// goxlsListen(data => {})
	js.Global().Set("goxlsListen", js.FuncOf(func(this js.Value, args []js.Value) any {
		s.jsListener = &args[0]
		return nil
	}))
	// goxlsEmit(data)
	js.Global().Set("goxlsEmit", js.FuncOf(func(this js.Value, args []js.Value) any {
		s.receive(&args[0])
		return nil
	}))
}

func (s *WebConn) write(ctx context.Context, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshaling msg: %v", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.jsListener != nil {
		return fmt.Errorf("no listener")
	}
	if s.jsListener.Type().String() != "function" {
		return fmt.Errorf("invalid listener")
	}
	s.jsListener.Invoke(string(data[:]))
	return nil
}

// Call invokes the target method and waits for a response.
// The params will be marshaled to JSON before sending over the wire, and will
// be handed to the method invoked.
// The response will be unmarshaled from JSON into the result.
// The id returned will be unique from this connection, and can be used for
// logging or tracking.
func (s *WebConn) Call(ctx context.Context, method string, params, result interface{}) error {
	// generate a new request identifier
	id := ID{number: atomic.AddInt64(&s.seq, 1)}
	call, err := NewCall(id, method, params)
	if err != nil {
		return fmt.Errorf("marshaling call parameters: %v", err)
	}
	// We have to add ourselves to the pending map before we send, otherwise we
	// are racing the response. Also add a buffer to rchan, so that if we get a
	// wire response between the time this call is cancelled and id is deleted
	// from s.pending, the send to rchan will not block.
	rchan := make(chan *Response, 1)
	s.pendingMu.Lock()
	s.pending[id] = rchan
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, id)
		s.pendingMu.Unlock()
	}()
	// now we are ready to send
	err = s.write(ctx, call)
	if err != nil {
		// sending failed, we will never get a response, so don't leave it pending
		return err
	}
	// now wait for the response
	select {
	case response := <-rchan:
		// is it an error response?
		if response.err != nil {
			return response.err
		}
		if result == nil || len(response.result) == 0 {
			return nil
		}
		if err := json.Unmarshal(response.result, result); err != nil {
			return fmt.Errorf("unmarshaling result: %v", err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Notify invokes the target method but does not wait for a response.
// The params will be marshaled to JSON before sending over the wire, and will
// be handed to the method invoked.
func (s *WebConn) Notify(ctx context.Context, method string, params interface{}) error {
	notify, err := NewNotification(method, params)
	if err != nil {
		return fmt.Errorf("marshaling notify parameters: %v", err)
	}
	return s.write(ctx, notify)
}

// WebClient implements protocol.ClientCloser
type WebClient struct {
	sender *WebConn
}

func (c *WebClient) LogTrace(ctx context.Context, params *protocol.LogTraceParams) error {
	return c.sender.Notify(ctx, "$/logTrace", params)
}
func (c *WebClient) Progress(ctx context.Context, params *protocol.ProgressParams) error {
	return c.sender.Notify(ctx, "$/progress", params)
}
func (c *WebClient) RegisterCapability(ctx context.Context, params *protocol.RegistrationParams) error {
	return c.sender.Call(ctx, "client/registerCapability", params, nil)
}
func (c *WebClient) UnregisterCapability(ctx context.Context, params *protocol.UnregistrationParams) error {
	return c.sender.Call(ctx, "client/unregisterCapability", params, nil)
}
func (c *WebClient) Event(ctx context.Context, params *interface{}) error {
	return c.sender.Notify(ctx, "telemetry/event", params)
}
func (c *WebClient) PublishDiagnostics(ctx context.Context, params *protocol.PublishDiagnosticsParams) error {
	return c.sender.Notify(ctx, "textDocument/publishDiagnostics", params)
}
func (c *WebClient) LogMessage(ctx context.Context, params *protocol.LogMessageParams) error {
	return c.sender.Notify(ctx, "window/logMessage", params)
}
func (c *WebClient) ShowDocument(ctx context.Context, params *protocol.ShowDocumentParams) (*protocol.ShowDocumentResult, error) {
	var result *protocol.ShowDocumentResult
	if err := c.sender.Call(ctx, "window/showDocument", params, &result); err != nil {
		return nil, err
	}
	return result, nil
}
func (c *WebClient) ShowMessage(ctx context.Context, params *protocol.ShowMessageParams) error {
	return c.sender.Notify(ctx, "window/showMessage", params)
}
func (c *WebClient) ShowMessageRequest(ctx context.Context, params *protocol.ShowMessageRequestParams) (*protocol.MessageActionItem, error) {
	var result *protocol.MessageActionItem
	if err := c.sender.Call(ctx, "window/showMessageRequest", params, &result); err != nil {
		return nil, err
	}
	return result, nil
}
func (c *WebClient) WorkDoneProgressCreate(ctx context.Context, params *protocol.WorkDoneProgressCreateParams) error {
	return c.sender.Call(ctx, "window/workDoneProgress/create", params, nil)
}
func (c *WebClient) ApplyEdit(ctx context.Context, params *protocol.ApplyWorkspaceEditParams) (*protocol.ApplyWorkspaceEditResult, error) {
	var result *protocol.ApplyWorkspaceEditResult
	if err := c.sender.Call(ctx, "workspace/applyEdit", params, &result); err != nil {
		return nil, err
	}
	return result, nil
}
func (c *WebClient) CodeLensRefresh(ctx context.Context) error {
	return c.sender.Call(ctx, "workspace/codeLens/refresh", nil, nil)
}
func (c *WebClient) Configuration(ctx context.Context, params *protocol.ParamConfiguration) ([]protocol.LSPAny, error) {
	var result []protocol.LSPAny
	if err := c.sender.Call(ctx, "workspace/configuration", params, &result); err != nil {
		return nil, err
	}
	return result, nil
}
func (c *WebClient) DiagnosticRefresh(ctx context.Context) error {
	return c.sender.Call(ctx, "workspace/diagnostic/refresh", nil, nil)
}
func (c *WebClient) InlayHintRefresh(ctx context.Context) error {
	return c.sender.Call(ctx, "workspace/inlayHint/refresh", nil, nil)
}
func (c *WebClient) InlineValueRefresh(ctx context.Context) error {
	return c.sender.Call(ctx, "workspace/inlineValue/refresh", nil, nil)
}
func (c *WebClient) SemanticTokensRefresh(ctx context.Context) error {
	return c.sender.Call(ctx, "workspace/semanticTokens/refresh", nil, nil)
}
func (c *WebClient) WorkspaceFolders(ctx context.Context) ([]protocol.WorkspaceFolder, error) {
	var result []protocol.WorkspaceFolder
	if err := c.sender.Call(ctx, "workspace/workspaceFolders", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}
func (c *WebClient) Close() error {
	// TODO
	return nil
}

func serverDispatch(ctx context.Context, server protocol.Server, reply Replier, r Request) (bool, error) {
	switch r.Method() {
	case "$/progress":
		var params protocol.ProgressParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		err := server.Progress(ctx, &params)
		return true, reply(ctx, nil, err)
	case "$/setTrace":
		var params protocol.SetTraceParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		err := server.SetTrace(ctx, &params)
		return true, reply(ctx, nil, err)
	case "callHierarchy/incomingCalls":
		var params protocol.CallHierarchyIncomingCallsParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.IncomingCalls(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "callHierarchy/outgoingCalls":
		var params protocol.CallHierarchyOutgoingCallsParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.OutgoingCalls(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "codeAction/resolve":
		var params protocol.CodeAction
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.ResolveCodeAction(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "codeLens/resolve":
		var params protocol.CodeLens
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.ResolveCodeLens(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "completionItem/resolve":
		var params protocol.CompletionItem
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.ResolveCompletionItem(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "documentLink/resolve":
		var params protocol.DocumentLink
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.ResolveDocumentLink(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "exit":
		err := server.Exit(ctx)
		return true, reply(ctx, nil, err)
	case "initialize":
		var params protocol.ParamInitialize
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.Initialize(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "initialized":
		var params protocol.InitializedParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		err := server.Initialized(ctx, &params)
		return true, reply(ctx, nil, err)
	case "inlayHint/resolve":
		var params protocol.InlayHint
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.Resolve(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "notebookDocument/didChange":
		var params protocol.DidChangeNotebookDocumentParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		err := server.DidChangeNotebookDocument(ctx, &params)
		return true, reply(ctx, nil, err)
	case "notebookDocument/didClose":
		var params protocol.DidCloseNotebookDocumentParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		err := server.DidCloseNotebookDocument(ctx, &params)
		return true, reply(ctx, nil, err)
	case "notebookDocument/didOpen":
		var params protocol.DidOpenNotebookDocumentParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		err := server.DidOpenNotebookDocument(ctx, &params)
		return true, reply(ctx, nil, err)
	case "notebookDocument/didSave":
		var params protocol.DidSaveNotebookDocumentParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		err := server.DidSaveNotebookDocument(ctx, &params)
		return true, reply(ctx, nil, err)
	case "shutdown":
		err := server.Shutdown(ctx)
		return true, reply(ctx, nil, err)
	case "textDocument/codeAction":
		var params protocol.CodeActionParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.CodeAction(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/codeLens":
		var params protocol.CodeLensParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.CodeLens(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/colorPresentation":
		var params protocol.ColorPresentationParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.ColorPresentation(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/completion":
		var params protocol.CompletionParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.Completion(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/declaration":
		var params protocol.DeclarationParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.Declaration(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/definition":
		var params protocol.DefinitionParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.Definition(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/diagnostic":
		var params string
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.Diagnostic(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/didChange":
		var params protocol.DidChangeTextDocumentParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		err := server.DidChange(ctx, &params)
		return true, reply(ctx, nil, err)
	case "textDocument/didClose":
		var params protocol.DidCloseTextDocumentParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		err := server.DidClose(ctx, &params)
		return true, reply(ctx, nil, err)
	case "textDocument/didOpen":
		var params protocol.DidOpenTextDocumentParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		err := server.DidOpen(ctx, &params)
		return true, reply(ctx, nil, err)
	case "textDocument/didSave":
		var params protocol.DidSaveTextDocumentParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		err := server.DidSave(ctx, &params)
		return true, reply(ctx, nil, err)
	case "textDocument/documentColor":
		var params protocol.DocumentColorParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.DocumentColor(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/documentHighlight":
		var params protocol.DocumentHighlightParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.DocumentHighlight(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/documentLink":
		var params protocol.DocumentLinkParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.DocumentLink(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/documentSymbol":
		var params protocol.DocumentSymbolParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.DocumentSymbol(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/foldingRange":
		var params protocol.FoldingRangeParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.FoldingRange(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/formatting":
		var params protocol.DocumentFormattingParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.Formatting(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/hover":
		var params protocol.HoverParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.Hover(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/implementation":
		var params protocol.ImplementationParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.Implementation(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/inlayHint":
		var params protocol.InlayHintParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.InlayHint(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/inlineCompletion":
		var params protocol.InlineCompletionParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.InlineCompletion(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/inlineValue":
		var params protocol.InlineValueParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.InlineValue(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/linkedEditingRange":
		var params protocol.LinkedEditingRangeParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.LinkedEditingRange(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/moniker":
		var params protocol.MonikerParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.Moniker(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/onTypeFormatting":
		var params protocol.DocumentOnTypeFormattingParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.OnTypeFormatting(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/prepareCallHierarchy":
		var params protocol.CallHierarchyPrepareParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.PrepareCallHierarchy(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/prepareRename":
		var params protocol.PrepareRenameParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.PrepareRename(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/prepareTypeHierarchy":
		var params protocol.TypeHierarchyPrepareParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.PrepareTypeHierarchy(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/rangeFormatting":
		var params protocol.DocumentRangeFormattingParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.RangeFormatting(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/rangesFormatting":
		var params protocol.DocumentRangesFormattingParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.RangesFormatting(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/references":
		var params protocol.ReferenceParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.References(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/rename":
		var params protocol.RenameParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.Rename(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/selectionRange":
		var params protocol.SelectionRangeParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.SelectionRange(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/semanticTokens/full":
		var params protocol.SemanticTokensParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.SemanticTokensFull(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/semanticTokens/full/delta":
		var params protocol.SemanticTokensDeltaParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.SemanticTokensFullDelta(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/semanticTokens/range":
		var params protocol.SemanticTokensRangeParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.SemanticTokensRange(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/signatureHelp":
		var params protocol.SignatureHelpParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.SignatureHelp(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/typeDefinition":
		var params protocol.TypeDefinitionParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.TypeDefinition(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "textDocument/willSave":
		var params protocol.WillSaveTextDocumentParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		err := server.WillSave(ctx, &params)
		return true, reply(ctx, nil, err)
	case "textDocument/willSaveWaitUntil":
		var params protocol.WillSaveTextDocumentParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.WillSaveWaitUntil(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "typeHierarchy/subtypes":
		var params protocol.TypeHierarchySubtypesParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.Subtypes(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "typeHierarchy/supertypes":
		var params protocol.TypeHierarchySupertypesParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.Supertypes(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "window/workDoneProgress/cancel":
		var params protocol.WorkDoneProgressCancelParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		err := server.WorkDoneProgressCancel(ctx, &params)
		return true, reply(ctx, nil, err)
	case "workspace/diagnostic":
		var params protocol.WorkspaceDiagnosticParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.DiagnosticWorkspace(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "workspace/didChangeConfiguration":
		var params protocol.DidChangeConfigurationParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		err := server.DidChangeConfiguration(ctx, &params)
		return true, reply(ctx, nil, err)
	case "workspace/didChangeWatchedFiles":
		var params protocol.DidChangeWatchedFilesParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		err := server.DidChangeWatchedFiles(ctx, &params)
		return true, reply(ctx, nil, err)
	case "workspace/didChangeWorkspaceFolders":
		var params protocol.DidChangeWorkspaceFoldersParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		err := server.DidChangeWorkspaceFolders(ctx, &params)
		return true, reply(ctx, nil, err)
	case "workspace/didCreateFiles":
		var params protocol.CreateFilesParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		err := server.DidCreateFiles(ctx, &params)
		return true, reply(ctx, nil, err)
	case "workspace/didDeleteFiles":
		var params protocol.DeleteFilesParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		err := server.DidDeleteFiles(ctx, &params)
		return true, reply(ctx, nil, err)
	case "workspace/didRenameFiles":
		var params protocol.RenameFilesParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		err := server.DidRenameFiles(ctx, &params)
		return true, reply(ctx, nil, err)
	case "workspace/executeCommand":
		var params protocol.ExecuteCommandParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.ExecuteCommand(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "workspace/symbol":
		var params protocol.WorkspaceSymbolParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.Symbol(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "workspace/willCreateFiles":
		var params protocol.CreateFilesParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.WillCreateFiles(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "workspace/willDeleteFiles":
		var params protocol.DeleteFilesParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.WillDeleteFiles(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "workspace/willRenameFiles":
		var params protocol.RenameFilesParams
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.WillRenameFiles(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	case "workspaceSymbol/resolve":
		var params protocol.WorkspaceSymbol
		if err := json.Unmarshal(r.Params(), &params); err != nil {
			return true, sendParseError(ctx, reply, err)
		}
		resp, err := server.ResolveWorkspaceSymbol(ctx, &params)
		if err != nil {
			return true, reply(ctx, nil, err)
		}
		return true, reply(ctx, resp, nil)
	default:
		return false, nil
	}
}

func ServerHandler(server protocol.Server) Handler {
	return func(ctx context.Context, reply Replier, req Request) error {
		if ctx.Err() != nil {
			ctx := xcontext.Detach(ctx)
			return reply(ctx, nil, ErrRequestCancelled)
		}
		handled, err := serverDispatch(ctx, server, reply, req)
		if handled || err != nil {
			return err
		}
		//TODO: This code is wrong, it ignores handler and assumes non standard
		// request handles everything
		// non standard request should just be a layered handler.
		var params interface{}
		if err := json.Unmarshal(req.Params(), &params); err != nil {
			return sendParseError(ctx, reply, err)
		}
		resp, err := server.NonstandardRequest(ctx, req.Method(), params)
		return reply(ctx, resp, err)
	}
}

func main() {
	ctx := context.TODO()

	c := &cache.Cache{}
	session := cache.NewSession(ctx, c, nil)
	conn := &WebConn{}
	client := &WebClient{conn}
	server := lsp.NewServer(session, client)
	handler := ServerHandler(server)
	conn.Run(handler)

	fmt.Println("Running...")

	<-make(chan bool)
}
