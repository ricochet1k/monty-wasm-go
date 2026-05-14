mod json;

use std::collections::HashMap;
use std::slice;
use std::str;
use std::sync::{Mutex, OnceLock};

use json::{
    decode_inputs, decode_object, decode_value, encode_kwargs, encode_object, encode_objects,
};
use monty::{
    bytecode::CallLocation,
    ExcType, ExtFunctionResult, FunctionCall, MontyException, MontyObject, MontyRepl, MontyRun,
    NameLookup, NameLookupResult, NoLimitTracker, OsCall, PrintWriter, ReplContinuationMode,
    ReplProgress, ResolveFutures, RunProgress, detect_repl_continuation_mode,
};
use postcard::{from_bytes, to_allocvec};
use serde::Deserialize;
use serde_json::{Value, json};
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

/// Wrapper enum for FunctionCall and OsCall snapshots
#[derive(serde::Serialize, serde::Deserialize)]
#[serde(tag = "type")]
enum SnapshotKind {
    #[serde(rename = "function_call")]
    FunctionCall(FunctionCall<NoLimitTracker>),
    #[serde(rename = "os_call")]
    OsCall(OsCall<NoLimitTracker>),
}

#[derive(Default)]
struct State {
    next_id: u64,
    runs: HashMap<u64, MontyRun>,
    function_snapshots: HashMap<u64, SnapshotKind>,
    future_snapshots: HashMap<u64, ResolveFutures<NoLimitTracker>>,
    name_lookup_snapshots: HashMap<u64, NameLookup<NoLimitTracker>>,
    repls: HashMap<u64, MontyRepl<NoLimitTracker>>,
    repl_progress: HashMap<u64, ReplProgress<NoLimitTracker>>,
    blobs: HashMap<u64, Vec<u8>>,
    last_error: Vec<u8>,
    last_repl_id: Option<u64>, // New REPL ID set after a ReplStartError
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
       RunProgress::FunctionCall(fc) => {
            let fn_name = fc.function_name.clone();
            let args_json = encode_objects(&fc.args)?;
            let kwargs_json = encode_kwargs(&fc.kwargs)?;
            let call_id = fc.call_id;
            let method_call = fc.method_call;
            let location = fc.location();
            let snapshot_id = state.alloc_id();
            state
                .function_snapshots
                .insert(snapshot_id, SnapshotKind::FunctionCall(fc));
            json!({
                "kind": "function_call",
                "function_name": fn_name,
                "args": serde_json::from_str::<Value>(&args_json)?,
                "kwargs": serde_json::from_str::<Value>(&kwargs_json)?,
                "call_id": call_id,
                "method_call": method_call,
                "snapshot_id": snapshot_id,
                "location": location,
            })
        }
      RunProgress::OsCall(os_call) => {
            let fn_name = os_call.function.to_string();
            let args_json = encode_objects(&os_call.args)?;
            let kwargs_json = encode_kwargs(&os_call.kwargs)?;
            let call_id = os_call.call_id;
            let location = os_call.location();
            let snapshot_id = state.alloc_id();
            state
                .function_snapshots
                .insert(snapshot_id, SnapshotKind::OsCall(os_call));
            json!({
                "kind": "os_call",
                "os_function": fn_name,
                "args": serde_json::from_str::<Value>(&args_json)?,
                "kwargs": serde_json::from_str::<Value>(&kwargs_json)?,
                "call_id": call_id,
                "snapshot_id": snapshot_id,
                "location": location,
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
        RunProgress::NameLookup(name_lookup) => {
            let name = name_lookup.name.clone();
            let snapshot_id = state.alloc_id();
            state.name_lookup_snapshots.insert(snapshot_id, name_lookup);
            json!({
                "kind": "name_lookup",
                "name": name,
                "snapshot_id": snapshot_id,
            })
        }
    };

    let bytes = serde_json::to_vec(&payload)?;
    Ok(store_blob(state, bytes))
}

fn encode_repl_progress(
    state: &mut State,
    progress: ReplProgress<NoLimitTracker>,
) -> BridgeResult<u64> {
    let payload = match progress {
        ReplProgress::Complete { repl: _, value } => {
            // Update REPL in state with the returned one
            // We don't know the repl_id here, so we store the value and update later
            let result_json = encode_object(&value)?;
            json!({
                "kind": "complete",
                "result": serde_json::from_str::<Value>(&result_json)?,
            })
        }
      ReplProgress::FunctionCall(fc) => {
            let fn_name = fc.function_name.clone();
            let args_json = encode_objects(&fc.args)?;
            let kwargs_json = encode_kwargs(&fc.kwargs)?;
            let call_id = fc.call_id;
            let method_call = fc.method_call;
            let location = fc.location();
            let snapshot_id = state.alloc_id();
            state
                .repl_progress
                .insert(snapshot_id, ReplProgress::FunctionCall(fc));
            json!({
                "kind": "function_call",
                "function_name": fn_name,
                "args": serde_json::from_str::<Value>(&args_json)?,
                "kwargs": serde_json::from_str::<Value>(&kwargs_json)?,
                "call_id": call_id,
                "method_call": method_call,
                "snapshot_id": snapshot_id,
                "location": location,
            })
        }
     ReplProgress::OsCall(os_call) => {
            let fn_name = os_call.function.to_string();
            let args_json = encode_objects(&os_call.args)?;
            let kwargs_json = encode_kwargs(&os_call.kwargs)?;
            let call_id = os_call.call_id;
            let location = os_call.location();
            let snapshot_id = state.alloc_id();
            state
                .repl_progress
                .insert(snapshot_id, ReplProgress::OsCall(os_call));
            json!({
                "kind": "os_call",
                "os_function": fn_name,
                "args": serde_json::from_str::<Value>(&args_json)?,
                "kwargs": serde_json::from_str::<Value>(&kwargs_json)?,
                "call_id": call_id,
                "snapshot_id": snapshot_id,
                "location": location,
            })
        }
        ReplProgress::ResolveFutures(snapshot) => {
            let ids = snapshot.pending_call_ids().to_vec();
            let future_snapshot_id = state.alloc_id();
            state
                .repl_progress
                .insert(future_snapshot_id, ReplProgress::ResolveFutures(snapshot));
            json!({
                "kind": "resolve_futures",
                "pending_call_ids": ids,
                "future_snapshot_id": future_snapshot_id,
            })
        }
        ReplProgress::NameLookup(name_lookup) => {
            let name = name_lookup.name.clone();
            let snapshot_id = state.alloc_id();
            state
                .repl_progress
                .insert(snapshot_id, ReplProgress::NameLookup(name_lookup));
            json!({
                "kind": "name_lookup",
                "name": name,
                "snapshot_id": snapshot_id,
            })
        }
    };

    let bytes = serde_json::to_vec(&payload)?;
    Ok(store_blob(state, bytes))
}

fn decode_future_results(raw: &str) -> BridgeResult<Vec<(u32, ExtFunctionResult)>> {
    let entries: Vec<FutureResultJson> = serde_json::from_str(raw)?;
    entries
        .into_iter()
        .map(|entry| {
            if let Some(err) = entry.error.filter(|e| !e.is_empty()) {
                return Ok((
                    entry.call_id,
                    ExtFunctionResult::Error(MontyException::new(ExcType::RuntimeError, Some(err))),
                ));
            }
            if let Some(value) = entry.result {
                return Ok((
                    entry.call_id,
                    ExtFunctionResult::Return(decode_value(value)?),
                ));
            }
            Err(BridgeError::Message(format!(
                "future result for call_id {} missing both 'result' and 'error'",
                entry.call_id
            )))
        })
        .collect()
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn monty_alloc(size: u32) -> u32 {
    if size == 0 {
        return 0;
    }
    let mut buf = Vec::<u8>::with_capacity(size as usize);
    let ptr = buf.as_mut_ptr();
    std::mem::forget(buf);
    ptr as u32
}

#[unsafe(no_mangle)]
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

#[unsafe(no_mangle)]
pub extern "C" fn monty_last_error_len() -> u32 {
    state().lock().unwrap().last_error.len() as u32
}

#[unsafe(no_mangle)]
pub extern "C" fn monty_last_error_ptr() -> u32 {
    let guard = state().lock().unwrap();
    if guard.last_error.is_empty() {
        return 0;
    }
    guard.last_error.as_ptr() as u32
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn monty_run_new(
    code_ptr: u32,
    code_len: u32,
    script_ptr: u32,
    script_len: u32,
    input_names_ptr: u32,
    input_names_len: u32,
) -> u64 {
    with_state(|state| {
        let code = read_string(code_ptr, code_len)?;
        let script = read_string(script_ptr, script_len)?;
        let input_names: Vec<String> =
            serde_json::from_slice(read_bytes(input_names_ptr, input_names_len)?)?;
        let run = MontyRun::new(code, &script, input_names)?;
        let id = state.alloc_id();
        state.runs.insert(id, run);
        Ok(id)
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn monty_run_start(run_id: u64, inputs_ptr: u32, inputs_len: u32) -> u64 {
    with_state(|state| {
        let run = state
            .runs
            .remove(&run_id)
            .ok_or_else(|| BridgeError::Message("unknown run id".into()))?;
        let inputs_raw = read_string(inputs_ptr, inputs_len)?;
        let inputs = decode_inputs(&inputs_raw)?;
        let progress = run.start(inputs, NoLimitTracker, PrintWriter::Stdout)?;
        encode_progress(state, progress)
    })
}

/// Snapshot type indicator for resume
#[derive(Debug, Deserialize)]
#[serde(rename_all = "snake_case")]
enum SnapshotType {
    FunctionCall,
    OsCall,
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn monty_snapshot_resume(
    snapshot_id: u64,
    _call_id: u32,
    mode: u32,
    result_ptr: u32,
    result_len: u32,
    err_ptr: u32,
    err_len: u32,
    snapshot_type_ptr: u32,
    snapshot_type_len: u32,
) -> u64 {
    with_state(|state| {
        let resolution = match mode {
            0 => {
                let value = read_string(result_ptr, result_len)?;
                ExtFunctionResult::Return(decode_object(&value)?)
            }
            1 => {
                let message = read_string(err_ptr, err_len)?;
                ExtFunctionResult::Error(MontyException::new(ExcType::RuntimeError, Some(message)))
            }
            2 => ExtFunctionResult::Future(_call_id),
            _ => return Err(BridgeError::Message("invalid resume mode".into())),
        };

        let snapshot_type_str = read_string(snapshot_type_ptr, snapshot_type_len)?;
        let snapshot_type: SnapshotType = serde_json::from_str(&snapshot_type_str)?;

        match snapshot_type {
            SnapshotType::FunctionCall => {
                let snapshot = state
                    .function_snapshots
                    .remove(&snapshot_id)
                    .ok_or_else(|| BridgeError::Message("unknown snapshot id".into()))?;
                match snapshot {
                    SnapshotKind::FunctionCall(snap) => {
                        let progress = snap.resume(resolution, PrintWriter::Stdout)?;
                        encode_progress(state, progress)
                    }
                    SnapshotKind::OsCall(_) => {
                        Err(BridgeError::Message("snapshot type mismatch".into()))
                    }
                }
            }
            SnapshotType::OsCall => {
                let snapshot = state
                    .function_snapshots
                    .remove(&snapshot_id)
                    .ok_or_else(|| BridgeError::Message("unknown snapshot id".into()))?;
                match snapshot {
                    SnapshotKind::OsCall(snap) => {
                        let progress = snap.resume(resolution, PrintWriter::Stdout)?;
                        encode_progress(state, progress)
                    }
                    SnapshotKind::FunctionCall(_) => {
                        Err(BridgeError::Message("snapshot type mismatch".into()))
                    }
                }
            }
        }
    })
}

#[unsafe(no_mangle)]
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
        let progress = snapshot.resume(results, PrintWriter::Stdout)?;
        encode_progress(state, progress)
    })
}

#[unsafe(no_mangle)]
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

#[unsafe(no_mangle)]
pub unsafe extern "C" fn monty_run_load(ptr: u32, len: u32) -> u64 {
    with_state(|state| {
        let bytes = read_bytes(ptr, len)?;
        let run = MontyRun::load(bytes)?;
        let id = state.alloc_id();
        state.runs.insert(id, run);
        Ok(id)
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn monty_snapshot_dump(snapshot_id: u64) -> u64 {
    with_state(|state| {
        let snapshot = state
            .function_snapshots
            .get(&snapshot_id)
            .ok_or_else(|| BridgeError::Message("unknown snapshot id".into()))?;
        let bytes = to_allocvec(snapshot)?;
        Ok(store_blob(state, bytes))
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn monty_snapshot_load(ptr: u32, len: u32) -> u64 {
    with_state(|state| {
        let bytes = read_bytes(ptr, len)?;
        let snapshot: SnapshotKind = from_bytes(bytes)?;
        let id = state.alloc_id();
        state.function_snapshots.insert(id, snapshot);
        Ok(id)
    })
}

#[unsafe(no_mangle)]
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

#[unsafe(no_mangle)]
pub unsafe extern "C" fn monty_future_snapshot_load(ptr: u32, len: u32) -> u64 {
    with_state(|state| {
        let bytes = read_bytes(ptr, len)?;
        let snapshot: ResolveFutures<NoLimitTracker> = from_bytes(bytes)?;
        let id = state.alloc_id();
        state.future_snapshots.insert(id, snapshot);
        Ok(id)
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn monty_name_lookup_dump(snapshot_id: u64) -> u64 {
    with_state(|state| {
        let snapshot = state
            .name_lookup_snapshots
            .get(&snapshot_id)
            .ok_or_else(|| BridgeError::Message("unknown name lookup id".into()))?;
        let bytes = to_allocvec(snapshot)?;
        Ok(store_blob(state, bytes))
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn monty_name_lookup_load(ptr: u32, len: u32) -> u64 {
    with_state(|state| {
        let bytes = read_bytes(ptr, len)?;
        let snapshot: NameLookup<NoLimitTracker> = from_bytes(bytes)?;
        let id = state.alloc_id();
        state.name_lookup_snapshots.insert(id, snapshot);
        Ok(id)
    })
}

#[unsafe(no_mangle)]
pub extern "C" fn monty_run_free(run_id: u64) {
    let mut state = state().lock().unwrap();
    state.runs.remove(&run_id);
}

#[unsafe(no_mangle)]
pub extern "C" fn monty_snapshot_free(snapshot_id: u64) {
    let mut state = state().lock().unwrap();
    state.function_snapshots.remove(&snapshot_id);
}

#[unsafe(no_mangle)]
pub extern "C" fn monty_future_snapshot_free(snapshot_id: u64) {
    let mut state = state().lock().unwrap();
    state.future_snapshots.remove(&snapshot_id);
}

#[unsafe(no_mangle)]
pub extern "C" fn monty_name_lookup_free(snapshot_id: u64) {
    let mut state = state().lock().unwrap();
    state.name_lookup_snapshots.remove(&snapshot_id);
}

#[unsafe(no_mangle)]
pub extern "C" fn monty_blob_ptr(blob_id: u64) -> u32 {
    let state = state().lock().unwrap();
    state.blobs.get(&blob_id).map_or(0, |b| b.as_ptr() as u32)
}

#[unsafe(no_mangle)]
pub extern "C" fn monty_blob_len(blob_id: u64) -> u32 {
    let state = state().lock().unwrap();
    state.blobs.get(&blob_id).map_or(0, |b| b.len() as u32)
}

#[unsafe(no_mangle)]
pub extern "C" fn monty_blob_free(blob_id: u64) {
    let mut state = state().lock().unwrap();
    state.blobs.remove(&blob_id);
}
// ============================================================
// REPL extern "C" functions
// ============================================================

/// Detect REPL continuation mode
/// Returns: 0=Complete, 1=IncompleteImplicit, 2=IncompleteBlock
#[unsafe(no_mangle)]
pub extern "C" fn monty_repl_check_continuation(code_ptr: u32, code_len: u32) -> u32 {
    let code = match str::from_utf8(unsafe {
        slice::from_raw_parts(code_ptr as *const u8, code_len as usize)
    }) {
        Ok(s) => s,
        Err(_) => return 0, // treat invalid UTF-8 as incomplete
    };
    match detect_repl_continuation_mode(code) {
        ReplContinuationMode::Complete => 0,
        ReplContinuationMode::IncompleteImplicit => 1,
        ReplContinuationMode::IncompleteBlock => 2,
    }
}

/// Create a new REPL
#[unsafe(no_mangle)]
pub unsafe extern "C" fn monty_repl_new(script_ptr: u32, script_len: u32) -> u64 {
    with_state(|state| {
        let script = read_string(script_ptr, script_len)?;
        let repl = MontyRepl::new(&script, NoLimitTracker);
        let id = state.alloc_id();
        state.repls.insert(id, repl);
        Ok(id)
    })
}

/// Start REPL execution with suspension support
/// Returns progress_id (0 on error)
#[unsafe(no_mangle)]
pub unsafe extern "C" fn monty_repl_start(
    repl_id: u64,
    code_ptr: u32,
    code_len: u32,
    input_names_ptr: u32,
    input_names_len: u32,
    inputs_ptr: u32,
    inputs_len: u32,
) -> u64 {
    with_state(|state| {
        let code = read_string(code_ptr, code_len)?;
        let input_names: Vec<String> =
            serde_json::from_slice(read_bytes(input_names_ptr, input_names_len)?)?;
        let inputs_values: Vec<MontyObject> = decode_inputs(&read_string(inputs_ptr, inputs_len)?)?;
        // Pair input names with values
        let inputs: Vec<(String, MontyObject)> = input_names
            .into_iter()
            .zip(inputs_values.into_iter())
            .collect();

        // Remove REPL from state to consume it
        let repl = state
            .repls
            .remove(&repl_id)
            .ok_or_else(|| BridgeError::Message("unknown repl id".into()))?;

        // feed_start consumes self and returns ReplProgress or ReplStartError
        match repl.feed_start(&code, inputs, PrintWriter::Stdout) {
            Ok(progress) => encode_repl_progress(state, progress),
            Err(start_err) => {
                // ReplStartError contains the REPL (still valid) and the error
                let err_str = start_err.error.summary();
                let repl = start_err.repl;
                // Store the REPL back in state with a new ID
                let new_repl_id = state.alloc_id();
                state.repls.insert(new_repl_id, repl);
                // Include the new REPL ID in the error message so Go can use it
                let full_err = format!("{} (repl_id={})", err_str, new_repl_id);
                state.set_error(full_err);
                // Store the old REPL ID in a separate field so Go can find it
                state.last_repl_id = Some(repl_id);
                Ok(0)
            }
        }
    })
}
/// Resume from a REPL progress snapshot
/// progress_type: 0=FunctionCall, 1=OsCall, 2=NameLookup, 3=ResolveFutures
#[unsafe(no_mangle)]
pub unsafe extern "C" fn monty_repl_resume(
    progress_id: u64,
    progress_type: u32,
    result_ptr: u32,
    result_len: u32,
) -> u64 {
    with_state(|state| {
        let progress = state
            .repl_progress
            .remove(&progress_id)
            .ok_or_else(|| BridgeError::Message("unknown repl progress id".into()))?;

        // Validate progress_type matches the actual variant
        let expected_type = match &progress {
            ReplProgress::FunctionCall(_) => 0,
            ReplProgress::OsCall(_) => 1,
            ReplProgress::NameLookup(_) => 2,
            ReplProgress::ResolveFutures(_) => 3,
            ReplProgress::Complete { .. } => {
                return Err(BridgeError::Message("cannot resume from Complete".into()));
            }
        };
        if progress_type != expected_type {
            return Err(BridgeError::Message(format!(
                "progress_type mismatch: expected {} but got {}",
                expected_type, progress_type
            )));
        }

        let result_json = read_string(result_ptr, result_len)?;
        let new_progress =
            match progress {
                ReplProgress::FunctionCall(fc) => {
                    let result: Value = serde_json::from_str(&result_json)?;
                    // Handle "future" type separately - it doesn't produce an ExtFunctionResult
                    if result.get("type").and_then(|v| v.as_str()) == Some("future") {
                        match fc.resume_pending(PrintWriter::Stdout) {
                            Ok(new_progress) => new_progress,
                            Err(start_err) => {
                                let err_str = start_err.error.summary();
                                let repl = start_err.repl;
                                let repl_id = state.alloc_id();
                                state.repls.insert(repl_id, repl);
                                state.set_error(err_str);
                                return Ok(0);
                            }
                        }
                    } else {
                        let ext_result = match result.get("type") {
                            Some(type_val) if type_val == "return" => {
                                if let Some(value) = result.get("value") {
                                    ExtFunctionResult::Return(decode_value(value.clone())?)
                                } else {
                                    return Err(BridgeError::Message(
                                        "function call result missing 'value'".into(),
                                    ));
                                }
                            }
                            Some(type_val) if type_val == "error" => {
                                if let Some(msg) = result.get("message").and_then(|v| v.as_str()) {
                                    ExtFunctionResult::Error(MontyException::new(
                                        ExcType::RuntimeError,
                                        Some(msg.to_string()),
                                    ))
                                } else {
                                    return Err(BridgeError::Message(
                                        "function call error result missing 'message'".into(),
                                    ));
                                }
                            }
                            _ => {
                                return Err(BridgeError::Message(
                                    "function call result missing 'type' field".into(),
                                ));
                            }
                        };
                        match fc.resume(ext_result, PrintWriter::Stdout) {
                            Ok(p) => p,
                            Err(start_err) => {
                                let err_str = start_err.error.summary();
                                let repl = start_err.repl;
                                let repl_id = state.alloc_id();
                                state.repls.insert(repl_id, repl);
                                state.set_error(err_str);
                                return Ok(0);
                            }
                        }
                    }
                }
                ReplProgress::OsCall(os_call) => {
                    let result: Value = serde_json::from_str(&result_json)?;
                    let ext_result = match result.get("type") {
                        Some(type_val) if type_val == "return" => {
                            if let Some(value) = result.get("value") {
                                ExtFunctionResult::Return(decode_value(value.clone())?)
                            } else {
                                return Err(BridgeError::Message(
                                    "os call result missing 'value'".into(),
                                ));
                            }
                        }
                        Some(type_val) if type_val == "error" => {
                            if let Some(msg) = result.get("message").and_then(|v| v.as_str()) {
                                ExtFunctionResult::Error(MontyException::new(
                                    ExcType::RuntimeError,
                                    Some(msg.to_string()),
                                ))
                            } else {
                                return Err(BridgeError::Message(
                                    "os call error result missing 'message'".into(),
                                ));
                            }
                        }
                        Some(type_val) if type_val == "future" => {
                            if let Some(call_id) = result.get("call_id").and_then(|v| v.as_u64()) {
                                ExtFunctionResult::Future(call_id as u32)
                            } else {
                                return Err(BridgeError::Message(
                                    "os call future result missing 'call_id'".into(),
                                ));
                            }
                        }
                        _ => {
                            return Err(BridgeError::Message(
                                "os call result missing 'type' field".into(),
                            ));
                        }
                    };
                    match os_call.resume(ext_result, PrintWriter::Stdout) {
                        Ok(p) => p,
                        Err(start_err) => {
                            let err_str = start_err.error.summary();
                            let repl = start_err.repl;
                            let repl_id = state.alloc_id();
                            state.repls.insert(repl_id, repl);
                            state.set_error(err_str);
                            return Ok(0);
                        }
                    }
                }
                ReplProgress::NameLookup(nl) => {
                    let result: Value = serde_json::from_str(&result_json)?;
                    let lookup_result = match result.get("type") {
                        Some(type_val) if type_val == "value" => {
                            if let Some(value) = result.get("value") {
                                NameLookupResult::Value(decode_value(value.clone())?)
                            } else {
                                return Err(BridgeError::Message(
                                    "name lookup result missing 'value'".into(),
                                ));
                            }
                        }
                        Some(type_val) if type_val == "function" => {
                            // Create an external function with the given name
                            if let Some(name) = result.get("name").and_then(|v| v.as_str()) {
                                NameLookupResult::Value(MontyObject::Function {
                                    name: name.to_string(),
                                    docstring: None,
                                })
                            } else {
                                return Err(BridgeError::Message(
                                    "name lookup function result missing 'name'".into(),
                                ));
                            }
                        }
                        Some(type_val) if type_val == "undefined" => NameLookupResult::Undefined,
                        _ => {
                            return Err(BridgeError::Message(
                                "name lookup result missing 'type' field".into(),
                            ));
                        }
                    };
                    match nl.resume(lookup_result, PrintWriter::Stdout) {
                        Ok(p) => p,
                        Err(start_err) => {
                            let err_str = start_err.error.summary();
                            let repl = start_err.repl;
                            let repl_id = state.alloc_id();
                            state.repls.insert(repl_id, repl);
                            state.set_error(err_str);
                            return Ok(0);
                        }
                    }
                }
                ReplProgress::ResolveFutures(snapshot) => {
                    let results: Vec<FutureResultJson> = serde_json::from_str(&result_json)?;
                    let resolved: Vec<(u32, ExtFunctionResult)> = results
                    .into_iter()
                    .map(|entry| {
                        if let Some(err) = entry.error.filter(|e| !e.is_empty()) {
                            Ok((
                                entry.call_id,
                                ExtFunctionResult::Error(MontyException::new(
                                    ExcType::RuntimeError,
                                    Some(err),
                                )),
                            ))
                        } else if let Some(value) = entry.result {
                            Ok((entry.call_id, ExtFunctionResult::Return(decode_value(value)?)))
                        } else {
                            Err(BridgeError::Message(format!(
                                "future result for call_id {} missing both 'result' and 'error'",
                                entry.call_id
                            )))
                        }
                    })
                  .collect::<Result<Vec<_>, _>>()?;
                    match snapshot.resume(resolved, PrintWriter::Stdout) {
                        Ok(p) => p,
                        Err(start_err) => {
                            let err_str = start_err.error.summary();
                            let repl = start_err.repl;
                            let repl_id = state.alloc_id();
                            state.repls.insert(repl_id, repl);
                            state.set_error(err_str);
                            return Ok(0);
                        }
                    }
                }
                ReplProgress::Complete { .. } => {
                    return Err(BridgeError::Message(
                        "cannot resume a complete progress".into(),
                    ));
                }
            };

        encode_repl_progress(state, new_progress)
    })
}

/// Simple blocking feed (no suspension, runs to completion)
#[unsafe(no_mangle)]
pub unsafe extern "C" fn monty_repl_feed(repl_id: u64, code_ptr: u32, code_len: u32) -> u64 {
    with_state(|state| {
        let code = read_string(code_ptr, code_len)?;
        let repl = state
            .repls
            .get_mut(&repl_id)
            .ok_or_else(|| BridgeError::Message("unknown repl id".into()))?;
        let result = repl.feed_run(&code, Vec::new(), PrintWriter::Stdout)?;
        if matches!(result, MontyObject::None) {
            Ok(0)
        } else {
            let result_json = encode_object(&result)?;
            Ok(store_blob(state, result_json.into_bytes()))
        }
    })
}

/// Dump REPL state to blob
#[unsafe(no_mangle)]
pub unsafe extern "C" fn monty_repl_dump(repl_id: u64) -> u64 {
    with_state(|state| {
        let repl = state
            .repls
            .get(&repl_id)
            .ok_or_else(|| BridgeError::Message("unknown repl id".into()))?;
        let bytes = repl.dump()?;
        Ok(store_blob(state, bytes))
    })
}

/// Load REPL state from bytes
#[unsafe(no_mangle)]
pub unsafe extern "C" fn monty_repl_load(ptr: u32, len: u32) -> u64 {
    with_state(|state| {
        let bytes = read_bytes(ptr, len)?;
        let repl = MontyRepl::load(bytes)?;
        let id = state.alloc_id();
        state.repls.insert(id, repl);
        Ok(id)
    })
}

/// Free REPL
#[unsafe(no_mangle)]
pub extern "C" fn monty_repl_free(repl_id: u64) {
    let mut state = state().lock().unwrap();
    state.repls.remove(&repl_id);
}

/// Free REPL progress snapshot
#[unsafe(no_mangle)]
pub extern "C" fn monty_repl_progress_free(progress_id: u64) {
    let mut state = state().lock().unwrap();
    state.repl_progress.remove(&progress_id);
}
/// Dump REPL progress snapshot to blob
#[unsafe(no_mangle)]
pub extern "C" fn monty_repl_snapshot_dump(snapshot_id: u64) -> u64 {
    with_state(|state| {
        let snapshot = state
            .repl_progress
            .get(&snapshot_id)
            .ok_or_else(|| BridgeError::Message("unknown repl snapshot id".into()))?;
        let bytes = to_allocvec(snapshot)?;
        Ok(store_blob(state, bytes))
    })
}

/// Load REPL progress snapshot from bytes
#[unsafe(no_mangle)]
pub unsafe extern "C" fn monty_repl_snapshot_load(ptr: u32, len: u32) -> u64 {
    with_state(|state| {
        let bytes = read_bytes(ptr, len)?;
        let snapshot: ReplProgress<NoLimitTracker> = from_bytes(bytes)?;
        let id = state.alloc_id();
        state.repl_progress.insert(id, snapshot);
        Ok(id)
    })
}

/// Free REPL progress snapshot
#[unsafe(no_mangle)]
pub extern "C" fn monty_repl_snapshot_free(snapshot_id: u64) {
    let mut state = state().lock().unwrap();
    state.repl_progress.remove(&snapshot_id);
}
