// Copyright 2022 The GoPlus Authors (goplus.org). All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// Message is the interface to all message types.
// They share no common functionality, but are a closed set of concrete types
// that are allowed to implement this interface. The message types are *Call,
// *Notification and *Response.
type Message interface{}

// ID is a Request identifier.
type ID struct {
	name   string
	number int64
}

// Request is the shared interface to messages that request
// a method be invoked.
// The request types are a closed set of *Call and *Notification.
type Request interface {
	Message
	// Method is a string containing the method name to invoke.
	Method() string
	// Params is either a struct or an array with the parameters of the method.
	Params() json.RawMessage
}

// Response is a reply to a Call.
// It will have the same ID as the call it is a response to.
type Response struct {
	// result is the content of the response.
	result json.RawMessage
	// err is set only if the call failed.
	err error
	// ID of the request this is a response to.
	id ID
}

// NewResponse constructs a new Response message that is a reply to the
// supplied. If err is set result may be ignored.
func NewResponse(id ID, result interface{}, err error) (*Response, error) {
	r, merr := marshalToRaw(result)
	return &Response{id: id, result: r, err: err}, merr
}

func (msg *Response) ID() ID                  { return msg.id }
func (msg *Response) Result() json.RawMessage { return msg.result }
func (msg *Response) Err() error              { return msg.err }

// Notification is a request for which a response cannot occur, and as such
// it has not ID.
type Notification struct {
	// Method is a string containing the method name to invoke.
	method string
	params json.RawMessage
}

func (n *Notification) Method() string          { return n.method }
func (n *Notification) Params() json.RawMessage { return n.params }

func marshalToRaw(obj interface{}) (json.RawMessage, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return json.RawMessage{}, err
	}
	return json.RawMessage(data), nil
}

// NewNotification constructs a new Notification message for the supplied
// method and parameters.
func NewNotification(method string, params interface{}) (*Notification, error) {
	p, merr := marshalToRaw(params)
	return &Notification{method: method, params: p}, merr
}

// Call is a request that expects a response.
// The response will have a matching ID.
type Call struct {
	// Method is a string containing the method name to invoke.
	method string
	// Params is either a struct or an array with the parameters of the method.
	params json.RawMessage
	// id of this request, used to tie the Response back to the request.
	id ID
}

func (c *Call) Method() string          { return c.method }
func (c *Call) Params() json.RawMessage { return c.params }
func (c *Call) ID() ID                  { return c.id }

func NewCall(id ID, method string, params interface{}) (*Call, error) {
	p, merr := marshalToRaw(params)
	return &Call{id: id, method: method, params: p}, merr
}

// wireCombined has all the fields of both Request and Response.
// We can decode this and then work out which it is.
type wireCombined struct {
	ID     *ID              `json:"id,omitempty"`
	Method string           `json:"method"`
	Params *json.RawMessage `json:"params,omitempty"`
	Result *json.RawMessage `json:"result,omitempty"`
	Error  *wireError       `json:"error,omitempty"`
}

// wireError represents a structured error in a Response.
type wireError struct {
	// Code is an error code indicating the type of failure.
	Code int64 `json:"code"`
	// Message is a short description of the error.
	Message string `json:"message"`
	// Data is optional structured data containing additional information about the error.
	Data *json.RawMessage `json:"data,omitempty"`
}

func NewError(code int64, message string) error {
	return &wireError{
		Code:    code,
		Message: message,
	}
}

func (err *wireError) Error() string {
	return err.Message
}

var (
	// ErrUnknown should be used for all non coded errors.
	ErrUnknown = NewError(-32001, "JSON RPC unknown error")
	// ErrParse is used when invalid JSON was received by the server.
	ErrParse = NewError(-32700, "JSON RPC parse error")
	//ErrInvalidRequest is used when the JSON sent is not a valid Request object.
	ErrInvalidRequest = NewError(-32600, "JSON RPC invalid request")
	// ErrMethodNotFound should be returned by the handler when the method does
	// not exist / is not available.
	ErrMethodNotFound = NewError(-32601, "JSON RPC method not found")
	// ErrInvalidParams should be returned by the handler when method
	// parameter(s) were invalid.
	ErrInvalidParams = NewError(-32602, "JSON RPC invalid params")
	// ErrInternal is not currently returned but defined for completeness.
	ErrInternal = NewError(-32603, "JSON RPC internal error")

	//ErrServerOverloaded is returned when a message was refused due to a
	//server being temporarily unable to accept any new messages.
	ErrServerOverloaded = NewError(-32000, "JSON RPC overloaded")
	// RequestCancelledError should be used when a request is cancelled early.
	ErrRequestCancelled = NewError(-32800, "JSON RPC cancelled")
)

func DecodeMessage(data []byte) (Message, error) {
	msg := wireCombined{}
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("unmarshaling jsonrpc message: %w", err)
	}
	if msg.Method == "" {
		// no method, should be a response
		if msg.ID == nil {
			return nil, ErrInvalidRequest
		}
		response := &Response{id: *msg.ID}
		if msg.Error != nil {
			response.err = msg.Error
		}
		if msg.Result != nil {
			response.result = *msg.Result
		}
		return response, nil
	}
	// has a method, must be a request
	if msg.ID == nil {
		// request with no ID is a notify
		notify := &Notification{method: msg.Method}
		if msg.Params != nil {
			notify.params = *msg.Params
		}
		return notify, nil
	}
	// request with an ID, must be a call
	call := &Call{method: msg.Method, id: *msg.ID}
	if msg.Params != nil {
		call.params = *msg.Params
	}
	return call, nil
}

// Handler is invoked to handle incoming requests.
// The Replier sends a reply to the request and must be called exactly once.
type Handler func(ctx context.Context, reply Replier, req Request) error

// Replier is passed to handlers to allow them to reply to the request.
// If err is set then result will be ignored.
type Replier func(ctx context.Context, result interface{}, err error) error

func sendParseError(ctx context.Context, reply Replier, err error) error {
	return reply(ctx, nil, fmt.Errorf("%w: %s", ErrParse, err))
}
