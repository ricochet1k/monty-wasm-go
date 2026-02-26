mod json;

use std::collections::HashMap;
use std::slice;
use std::str;
use std::sync::{Mutex, OnceLock};

use json::{
    decode_inputs, decode_object, decode_value, encode_kwargs, encode_object, encode_objects,
};
use monty::{
    ExcType, ExternalResult, FutureSnapshot, MontyException, MontyRun, NoLimitTracker, PrintWriter,
    RunProgress, Snapshot,
};
use postcard::{from_bytes, to_allocvec};
use serde::Deserialize;
use serde_json::{json, Value};
use thiserror::Error;

type BridgeResult<T> = Result<T, BridgeError>;

#[derive(Debug, Error)]
enum BridgeError {
    #[error("{0}")]
    Message(String),
}

impl From<MontyException> for BridgeError {
    fn from(exc: MontyException) -> Self {
        Self::Message(exc.summary())
    }
}

impl From<serde_json::Error> for BridgeError {
    fn from(err: serde_json::Error) -> Self {
        Self::Message(err.to_string())
    }
}

impl From<postcard::Error> for BridgeError {
    fn from(err: postcard::Error) -> Self {
        Self::Message(err.to_string())
    }
}

impl From<std::str::Utf8Error> for BridgeError {
    fn from(err: std::str::Utf8Error) -> Self {
        Self::Message(err.to_string())
    }
}

#[derive(Debug, Deserialize)]
struct FutureResultJson {
    call_id: u32,
    #[serde(default)]
    result: Option<Value>,
    #[serde(default)]
    error: Option<String>,
}

#[derive(Default)]
struct State {
    next_id: u64,
    runs: HashMap<u64, MontyRun>,
    snapshots: HashMap<u64, Snapshot<NoLimitTracker>>,
    future_snapshots: HashMap<u64, FutureSnapshot<NoLimitTracker>>,
    blobs: HashMap<u64, Vec<u8>>,
    last_error: Vec<u8>,
}

impl State {
    fn alloc_id(&mut self) -> u64 {
        self.next_id += 1;
        self.next_id
    }

    fn set_error(&mut self, err: impl ToString) {
        self.last_error = err.to_string().into_bytes();
    }

    fn clear_error(&mut self) {
        self.last_error.clear();
    }
}

fn state() -> &'static Mutex<State> {
    static STATE: OnceLock<Mutex<State>> = OnceLock::new();
    STATE.get_or_init(|| Mutex::new(State::default()))
}

unsafe fn read_bytes(ptr: u32, len: u32) -> BridgeResult<&'static [u8]> {
    if len == 0 {
        return Ok(&[]);
    }
    if ptr == 0 {
        return Err(BridgeError::Message("null pointer".into()));
    }
    Ok(slice::from_raw_parts(ptr as *const u8, len as usize))
}

unsafe fn read_string(ptr: u32, len: u32) -> BridgeResult<String> {
    let bytes = read_bytes(ptr, len)?;
    Ok(str::from_utf8(bytes)?.to_owned())
}

fn with_state<F>(f: F) -> u64
where
    F: FnOnce(&mut State) -> BridgeResult<u64>,
{
    let mut guard = state().lock().unwrap();
    match f(&mut guard) {
        Ok(value) => {
            guard.clear_error();
            value
        }
        Err(err) => {
            guard.set_error(err);
            0
        }
    }
}

fn store_blob(state: &mut State, data: Vec<u8>) -> u64 {
    let id = state.alloc_id();
    state.blobs.insert(id, data);
    id
}

fn encode_progress(state: &mut State, progress: RunProgress<NoLimitTracker>) -> BridgeResult<u64> {
    let payload = match progress {
        RunProgress::Complete(value) => json!({
            "kind": "complete",
            "result": serde_json::from_str::<Value>(&encode_object(&value)?)?,
        }),
        RunProgress::FunctionCall {
            function_name,
            args,
            kwargs,
            call_id,
            method_call,
            state: snapshot,
        } => {
            let snapshot_id = state.alloc_id();
            state.snapshots.insert(snapshot_id, snapshot);
            json!({
                "kind": "function_call",
                "function_name": function_name,
                "args": serde_json::from_str::<Value>(&encode_objects(&args)?)?,
                "kwargs": serde_json::from_str::<Value>(&encode_kwargs(&kwargs)?)?,
                "call_id": call_id,
                "method_call": method_call,
                "snapshot_id": snapshot_id,
            })
        }
        RunProgress::OsCall {
            function,
            args,
            kwargs,
            call_id,
            state: snapshot,
        } => {
            let snapshot_id = state.alloc_id();
            state.snapshots.insert(snapshot_id, snapshot);
            json!({
                "kind": "os_call",
                "os_function": function.to_string(),
                "args": serde_json::from_str::<Value>(&encode_objects(&args)?)?,
                "kwargs": serde_json::from_str::<Value>(&encode_kwargs(&kwargs)?)?,
                "call_id": call_id,
                "snapshot_id": snapshot_id,
            })
        }
        RunProgress::ResolveFutures(snapshot) => {
            let ids = snapshot.pending_call_ids().to_vec();
            let future_snapshot_id = state.alloc_id();
            state.future_snapshots.insert(future_snapshot_id, snapshot);
            json!({
                "kind": "resolve_futures",
                "pending_call_ids": ids,
                "future_snapshot_id": future_snapshot_id,
            })
        }
    };

    let bytes = serde_json::to_vec(&payload)?;
    Ok(store_blob(state, bytes))
}

fn decode_future_results(raw: &str) -> BridgeResult<Vec<(u32, ExternalResult)>> {
    let entries: Vec<FutureResultJson> = serde_json::from_str(raw)?;
    entries
        .into_iter()
        .map(|entry| {
            if let Some(err) = entry.error.filter(|e| !e.is_empty()) {
                return Ok((
                    entry.call_id,
                    ExternalResult::Error(MontyException::new(ExcType::RuntimeError, Some(err))),
                ));
            }
            if let Some(value) = entry.result {
                return Ok((entry.call_id, ExternalResult::Return(decode_value(value)?)));
            }
            Ok((entry.call_id, ExternalResult::Future))
        })
        .collect()
}

#[no_mangle]
pub unsafe extern "C" fn monty_alloc(size: u32) -> u32 {
    if size == 0 {
        return 0;
    }
    let mut buf = Vec::<u8>::with_capacity(size as usize);
    let ptr = buf.as_mut_ptr();
    std::mem::forget(buf);
    ptr as u32
}

#[no_mangle]
pub unsafe extern "C" fn monty_free(ptr: u32, len: u32) {
    if ptr == 0 || len == 0 {
        return;
    }
    drop(Vec::from_raw_parts(
        ptr as *mut u8,
        len as usize,
        len as usize,
    ));
}

#[no_mangle]
pub extern "C" fn monty_last_error_len() -> u32 {
    state().lock().unwrap().last_error.len() as u32
}

#[no_mangle]
pub extern "C" fn monty_last_error_ptr() -> u32 {
    let guard = state().lock().unwrap();
    if guard.last_error.is_empty() {
        return 0;
    }
    guard.last_error.as_ptr() as u32
}

#[no_mangle]
pub unsafe extern "C" fn monty_run_new(
    code_ptr: u32,
    code_len: u32,
    script_ptr: u32,
    script_len: u32,
    input_names_ptr: u32,
    input_names_len: u32,
    ext_funcs_ptr: u32,
    ext_funcs_len: u32,
) -> u64 {
    with_state(|state| {
        let code = read_string(code_ptr, code_len)?;
        let script = read_string(script_ptr, script_len)?;
        let input_names: Vec<String> =
            serde_json::from_slice(read_bytes(input_names_ptr, input_names_len)?)?;
        let ext_funcs: Vec<String> =
            serde_json::from_slice(read_bytes(ext_funcs_ptr, ext_funcs_len)?)?;
        let run = MontyRun::new(code, &script, input_names, ext_funcs)?;
        let id = state.alloc_id();
        state.runs.insert(id, run);
        Ok(id)
    })
}

#[no_mangle]
pub unsafe extern "C" fn monty_run_start(run_id: u64, inputs_ptr: u32, inputs_len: u32) -> u64 {
    with_state(|state| {
        let run = state
            .runs
            .get(&run_id)
            .ok_or_else(|| BridgeError::Message("unknown run id".into()))?;
        let inputs_raw = read_string(inputs_ptr, inputs_len)?;
        let inputs = decode_inputs(&inputs_raw)?;
        let mut print = PrintWriter::Stdout;
        let progress = run.clone().start(inputs, NoLimitTracker, &mut print)?;
        encode_progress(state, progress)
    })
}

#[no_mangle]
pub unsafe extern "C" fn monty_snapshot_resume(
    snapshot_id: u64,
    _call_id: u32,
    mode: u32,
    result_ptr: u32,
    result_len: u32,
    err_ptr: u32,
    err_len: u32,
) -> u64 {
    with_state(|state| {
        let snapshot = state
            .snapshots
            .remove(&snapshot_id)
            .ok_or_else(|| BridgeError::Message("unknown snapshot id".into()))?;

        let resolution = match mode {
            0 => {
                let value = read_string(result_ptr, result_len)?;
                ExternalResult::Return(decode_object(&value)?)
            }
            1 => {
                let message = read_string(err_ptr, err_len)?;
                ExternalResult::Error(MontyException::new(ExcType::RuntimeError, Some(message)))
            }
            2 => ExternalResult::Future,
            _ => return Err(BridgeError::Message("invalid resume mode".into())),
        };

        let mut print = PrintWriter::Stdout;
        let progress = snapshot.run(resolution, &mut print)?;
        encode_progress(state, progress)
    })
}

#[no_mangle]
pub unsafe extern "C" fn monty_future_snapshot_resume(
    snapshot_id: u64,
    results_ptr: u32,
    results_len: u32,
) -> u64 {
    with_state(|state| {
        let snapshot = state
            .future_snapshots
            .remove(&snapshot_id)
            .ok_or_else(|| BridgeError::Message("unknown future snapshot id".into()))?;
        let raw = read_string(results_ptr, results_len)?;
        let results = decode_future_results(&raw)?;
        let mut print = PrintWriter::Stdout;
        let progress = snapshot.resume(results, &mut print)?;
        encode_progress(state, progress)
    })
}

#[no_mangle]
pub unsafe extern "C" fn monty_run_dump(run_id: u64) -> u64 {
    with_state(|state| {
        let run = state
            .runs
            .get(&run_id)
            .ok_or_else(|| BridgeError::Message("unknown run id".into()))?;
        let bytes = run.dump()?;
        Ok(store_blob(state, bytes))
    })
}

#[no_mangle]
pub unsafe extern "C" fn monty_run_load(ptr: u32, len: u32) -> u64 {
    with_state(|state| {
        let bytes = read_bytes(ptr, len)?;
        let run = MontyRun::load(bytes)?;
        let id = state.alloc_id();
        state.runs.insert(id, run);
        Ok(id)
    })
}

#[no_mangle]
pub unsafe extern "C" fn monty_snapshot_dump(snapshot_id: u64) -> u64 {
    with_state(|state| {
        let snapshot = state
            .snapshots
            .get(&snapshot_id)
            .ok_or_else(|| BridgeError::Message("unknown snapshot id".into()))?;
        let bytes = to_allocvec(snapshot)?;
        Ok(store_blob(state, bytes))
    })
}

#[no_mangle]
pub unsafe extern "C" fn monty_snapshot_load(ptr: u32, len: u32) -> u64 {
    with_state(|state| {
        let bytes = read_bytes(ptr, len)?;
        let snapshot: Snapshot<NoLimitTracker> = from_bytes(bytes)?;
        let id = state.alloc_id();
        state.snapshots.insert(id, snapshot);
        Ok(id)
    })
}

#[no_mangle]
pub unsafe extern "C" fn monty_future_snapshot_dump(snapshot_id: u64) -> u64 {
    with_state(|state| {
        let snapshot = state
            .future_snapshots
            .get(&snapshot_id)
            .ok_or_else(|| BridgeError::Message("unknown future snapshot id".into()))?;
        let bytes = to_allocvec(snapshot)?;
        Ok(store_blob(state, bytes))
    })
}

#[no_mangle]
pub unsafe extern "C" fn monty_future_snapshot_load(ptr: u32, len: u32) -> u64 {
    with_state(|state| {
        let bytes = read_bytes(ptr, len)?;
        let snapshot: FutureSnapshot<NoLimitTracker> = from_bytes(bytes)?;
        let id = state.alloc_id();
        state.future_snapshots.insert(id, snapshot);
        Ok(id)
    })
}

#[no_mangle]
pub extern "C" fn monty_run_free(run_id: u64) {
    let mut state = state().lock().unwrap();
    state.runs.remove(&run_id);
}

#[no_mangle]
pub extern "C" fn monty_snapshot_free(snapshot_id: u64) {
    let mut state = state().lock().unwrap();
    state.snapshots.remove(&snapshot_id);
}

#[no_mangle]
pub extern "C" fn monty_future_snapshot_free(snapshot_id: u64) {
    let mut state = state().lock().unwrap();
    state.future_snapshots.remove(&snapshot_id);
}

#[no_mangle]
pub extern "C" fn monty_blob_ptr(blob_id: u64) -> u32 {
    let state = state().lock().unwrap();
    state.blobs.get(&blob_id).map_or(0, |b| b.as_ptr() as u32)
}

#[no_mangle]
pub extern "C" fn monty_blob_len(blob_id: u64) -> u32 {
    let state = state().lock().unwrap();
    state.blobs.get(&blob_id).map_or(0, |b| b.len() as u32)
}

#[no_mangle]
pub extern "C" fn monty_blob_free(blob_id: u64) {
    let mut state = state().lock().unwrap();
    state.blobs.remove(&blob_id);
}
