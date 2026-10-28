use circular_buffer::CircularBuffer;
use rust_embed::Embed;
use std::env;
use std::fs;
use std::io::{self, Cursor, Read, Write};
use std::path::Path;
use std::sync::Arc;
use std::time::Instant;
use tract_core::internal::{RunnableModel, TVec, TypedFact, TypedOp};
use tract_core::prelude::multithread::{self, Executor};
use tract_core::prelude::{Framework, TValue};
use tract_onnx::prelude::*;

#[derive(Embed)]
#[folder = "models/"]
struct Models;

#[derive(Embed)]
#[folder = "speech_models/"]
struct SpeechModels;

type ModelType = Arc<RunnableModel<TypedFact, Box<dyn TypedOp>>>;

pub const OWW_MODEL_CHUNK_SIZE: usize = 1280; // 80 ms at 16kHz
pub const FEATURE_BUFFER_SIZE: usize = 16;
pub const MEL_LOOKBACK: usize = 160 * 3; // 480 samples
pub const MEL_INPUT_SIZE: usize = MEL_LOOKBACK + OWW_MODEL_CHUNK_SIZE; // 1760 samples
pub const MELS_PER_CHUNK: usize = MEL_INPUT_SIZE / 160 - 3; // 8 mel frames
pub const MEL_CIRC_SIZE: usize = 80 / MELS_PER_CHUNK; // 10
pub const DETECTION_BUFFER_SIZE: usize = 12;

pub struct AudioFeatures {
    mel: ModelType,
    emb: ModelType,
    raw_lookback: Vec<f32>,
    pub feature_buffer: Box<CircularBuffer<FEATURE_BUFFER_SIZE, Tensor>>,
    pub mel_spectrogram_buffer: Box<CircularBuffer<MEL_CIRC_SIZE, Tensor>>,
}

impl AudioFeatures {
    pub fn new(models_dir: Option<&str>) -> Result<Self, Box<dyn std::error::Error>> {
        multithread::set_default_executor(Executor::SingleThread);

        let mel_model_data: Vec<u8> = if let Some(dir) = models_dir {
            let p = Path::new(dir).join("melspectrogram.onnx");
            if p.exists() {
                fs::read(p)?
            } else {
                Models::get("melspectrogram.onnx").unwrap().data.into_owned()
            }
        } else {
            Models::get("melspectrogram.onnx").unwrap().data.into_owned()
        };

        let emb_model_data: Vec<u8> = if let Some(dir) = models_dir {
            let p = Path::new(dir).join("embedding_model.onnx");
            if p.exists() {
                fs::read(p)?
            } else {
                Models::get("embedding_model.onnx").unwrap().data.into_owned()
            }
        } else {
            Models::get("embedding_model.onnx").unwrap().data.into_owned()
        };

        let mut mel_rdr = Cursor::new(mel_model_data);
        let mel_model = tract_onnx::onnx().model_for_read(&mut mel_rdr)?;
        let mel = mel_model
            .with_input_fact(0, f32::fact([1, MEL_INPUT_SIZE]).into())?
            .into_optimized()?
            .into_runnable()?;

        let mut emb_rdr = Cursor::new(emb_model_data);
        let emb_model = tract_onnx::onnx().model_for_read(&mut emb_rdr)?;
        let emb = emb_model
            .with_input_fact(0, f32::fact([1, 76, 32, 1]).into())?
            .into_optimized()?
            .into_runnable()?;

        let mut feature_buffer = CircularBuffer::<FEATURE_BUFFER_SIZE, Tensor>::boxed();
        for _ in 0..FEATURE_BUFFER_SIZE {
            feature_buffer.push_back(Tensor::from_shape(&[1, 1, 1, 96], &[0f32; 96])?);
        }

        let mut mel_spectrogram_buffer = CircularBuffer::<MEL_CIRC_SIZE, Tensor>::boxed();
        for _ in 0..MEL_CIRC_SIZE {
            mel_spectrogram_buffer.push_back(Tensor::from_shape(
                &[MELS_PER_CHUNK, 32],
                &[0f32; MELS_PER_CHUNK * 32],
            )?);
        }

        Ok(AudioFeatures {
            mel,
            emb,
            raw_lookback: vec![0f32; MEL_LOOKBACK],
            feature_buffer,
            mel_spectrogram_buffer,
        })
    }

    pub fn get_features(&mut self, chunk: &[f32]) -> Result<Tensor, Box<dyn std::error::Error>> {
        let mut input = Vec::with_capacity(MEL_INPUT_SIZE);
        input.extend_from_slice(&self.raw_lookback);
        input.extend_from_slice(chunk);
        self.raw_lookback
            .copy_from_slice(&chunk[chunk.len() - MEL_LOOKBACK..]);

        let tensor = Tensor::from_shape(&[1, MEL_INPUT_SIZE], &input)?;
        let outputs: TVec<TValue> = self.mel.run(tvec!(tensor.into()))?;
        let out_tensor = outputs[0].clone().into_tensor();
        let resized = out_tensor.into_shape(&[MELS_PER_CHUNK, 32])?;
        let a = resized.into_plain_array::<f32>()?.into_owned();
        let mel_scaled = a.mapv(|v| (v / 10.0) + 2.0).into_tensor();

        self.mel_spectrogram_buffer.push_back(mel_scaled);
        let stacked_mels =
            Tensor::stack_tensors(0, &self.mel_spectrogram_buffer.to_vec())?;
        let slice = stacked_mels.slice(0, 4, 80)?;
        let reshaped = slice.into_shape(&[1, 76, 32, 1])?;

        let embeddings = self.emb.run(tvec!(reshaped.into()))?;
        self.feature_buffer
            .push_back(embeddings[0].clone().into_tensor());

        let stacked = Tensor::stack_tensors(0, &self.feature_buffer.to_vec())?;
        let features = stacked.into_shape(&[self.feature_buffer.len(), 96])?;
        Ok(features)
    }
}

pub struct WakeWordDetector {
    pub name: String,
    model: ModelType,
    threshold: f32,
    refractory_ms: u128,
    last_trigger: Instant,
    detections: CircularBuffer<DETECTION_BUFFER_SIZE, f32>,
}

impl WakeWordDetector {
    pub fn new(
        name: &str,
        model_data: &[u8],
        threshold: f32,
        refractory_ms: u128,
    ) -> Result<Self, Box<dyn std::error::Error>> {
        multithread::set_default_executor(Executor::SingleThread);
        let mut rdr = Cursor::new(model_data);
        let tract_model = tract_onnx::onnx()
            .model_for_read(&mut rdr)?
            .into_optimized()?
            .into_runnable()?;

        Ok(WakeWordDetector {
            name: name.to_string(),
            model: tract_model,
            threshold,
            refractory_ms,
            last_trigger: Instant::now() - std::time::Duration::from_secs(60),
            detections: CircularBuffer::new(),
        })
    }

    pub fn process(&mut self, features: &Tensor) -> (bool, f32) {
        let last = features.clone().into_shape(&[1, 16, 96]).unwrap();
        let out: TVec<TValue> = match self.model.run(tvec!(last.into())) {
            Ok(o) => o,
            Err(_) => return (false, 0.0),
        };
        let t = out[0].clone().into_tensor().cast_to::<f32>().unwrap().into_owned();
        let prob = t.into_plain_array::<f32>().unwrap().as_slice().unwrap()[0];

        self.detections.push_back(prob);

        let avg = self.calculate_average();
        let elapsed = self.last_trigger.elapsed().as_millis();

        // Responsive dual-trigger logic:
        // 1. Direct high-confidence frame: prob > (threshold + 0.15) (e.g. > 0.50 if threshold is 0.35)
        // 2. Smoothed average of multiple positive frames: avg > threshold
        let direct_trigger = prob > (self.threshold + 0.15);
        let smoothed_trigger = avg > self.threshold;

        if (direct_trigger || smoothed_trigger) && elapsed > self.refractory_ms {
            self.last_trigger = Instant::now();
            self.detections.clear();
            return (true, prob.max(avg));
        }
        (false, prob)
    }

    fn calculate_average(&self) -> f32 {
        let all = self.detections.to_vec();
        let mut sum = 0.0f32;
        let mut count = 0.0f32;
        for d in all {
            if d > self.threshold {
                count += 1.0;
                sum += d;
            }
        }
        if count < 1.0 {
            return 0.0;
        }
        sum / count
    }
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let args: Vec<String> = env::args().collect();
    let mut threshold = 0.35f32;
    let mut refractory_ms = 2000u128;
    let mut models_dir: Option<String> = None;
    let mut active_words: Vec<String> = vec!["alexa".to_string(), "jarvis".to_string()];

    let mut i = 1;
    while i < args.len() {
        match args[i].as_str() {
            "-t" | "--threshold" => {
                if i + 1 < args.len() {
                    threshold = args[i + 1].parse().unwrap_or(0.35);
                    i += 1;
                }
            }
            "-r" | "--refractory" => {
                if i + 1 < args.len() {
                    refractory_ms = args[i + 1].parse().unwrap_or(2000);
                    i += 1;
                }
            }
            "-d" | "--models-dir" => {
                if i + 1 < args.len() {
                    models_dir = Some(args[i + 1].clone());
                    i += 1;
                }
            }
            "-w" | "--word" | "--model" => {
                if i + 1 < args.len() {
                    active_words = args[i + 1].split(',').map(|s| s.trim().to_lowercase()).collect();
                    i += 1;
                }
            }
            "-h" | "--help" => {
                eprintln!("Usage: station-spotter [options]");
                eprintln!("  -w, --word <words>     Comma-separated: alexa, jarvis, mycroft, or path to .onnx");
                eprintln!("  -t, --threshold <val>  Detection threshold (default 0.35)");
                eprintln!("  -r, --refractory <ms>  Refractory cooldown in ms (default 2000)");
                eprintln!("  -d, --models-dir <dir> Optional directory with external .onnx models");
                return Ok(());
            }
            _ => {}
        }
        i += 1;
    }

    eprintln!("[Spotter] Initializing AudioFeatures (melspectrogram + speech embeddings)...");
    let mut audio = AudioFeatures::new(models_dir.as_deref())?;

    let mut detectors: Vec<WakeWordDetector> = Vec::new();

    for w in &active_words {
        let (name, data) = match w.as_str() {
            "alexa" => {
                let bytes = SpeechModels::get("alexa.onnx").unwrap().data.into_owned();
                ("alexa", bytes)
            }
            "jarvis" | "hey_jarvis" => {
                let bytes = SpeechModels::get("hey_jarvis_v0.1.onnx").unwrap().data.into_owned();
                ("jarvis", bytes)
            }
            "mycroft" | "hey_mycroft" => {
                let bytes = SpeechModels::get("hey_mycroft_v0.1.onnx").unwrap().data.into_owned();
                ("mycroft", bytes)
            }
            custom_path => {
                if let Ok(bytes) = fs::read(custom_path) {
                    let stem = Path::new(custom_path)
                        .file_stem()
                        .and_then(|s| s.to_str())
                        .unwrap_or("custom");
                    (stem, bytes)
                } else if let Some(ref d) = models_dir {
                    let p = Path::new(d).join(format!("{}.onnx", custom_path));
                    if let Ok(bytes) = fs::read(&p) {
                        (custom_path, bytes)
                    } else {
                        eprintln!("[Spotter] Warning: unknown model {}", custom_path);
                        continue;
                    }
                } else {
                    eprintln!("[Spotter] Warning: unknown model {}", custom_path);
                    continue;
                }
            }
        };

        eprintln!("[Spotter] Loading wake word detector '{}' (thresh: {:.2})...", name, threshold);
        let det = WakeWordDetector::new(name, &data, threshold, refractory_ms)?;
        detectors.push(det);
    }

    if detectors.is_empty() {
        eprintln!("[Spotter] Error: No wake word detectors initialized!");
        return Ok(());
    }

    eprintln!("[Spotter] Ready. Reading 16kHz mono PCM (1280 samples / 2560 bytes per chunk) from stdin...");
    let mut stdin = io::stdin();
    let mut stdout = io::stdout();

    let mut raw_buf = [0u8; OWW_MODEL_CHUNK_SIZE * 2];
    let mut float_chunk = vec![0f32; OWW_MODEL_CHUNK_SIZE];

    let mut chunk_counter: u64 = 0;

    loop {
        if let Err(e) = stdin.read_exact(&mut raw_buf) {
            if e.kind() == io::ErrorKind::UnexpectedEof {
                eprintln!("[Spotter] Stdin closed (EOF), exiting.");
                break;
            }
            eprintln!("[Spotter] Stdin read error: {}", e);
            break;
        }

        chunk_counter += 1;

        // Convert 16-bit LE PCM to f32 normalized [-1.0, 1.0] and compute RMS for heartbeat
        let mut sum_sq = 0.0f64;
        for (idx, slot) in float_chunk.iter_mut().enumerate() {
            let b0 = raw_buf[idx * 2] as u16;
            let b1 = raw_buf[idx * 2 + 1] as u16;
            let val = (b0 | (b1 << 8)) as i16;
            *slot = val as f32 / 32768.0;
            sum_sq += (val as f64) * (val as f64);
        }

        // Periodic heartbeat every 5 seconds (approx 62 chunks of 80ms)
        if chunk_counter % 62 == 0 {
            let rms = (sum_sq / (OWW_MODEL_CHUNK_SIZE as f64)).sqrt();
            eprintln!("[Spotter Heartbeat] Alive, audio RMS: {:.1}", rms);
        }

        // Compute features
        let features = match audio.get_features(&float_chunk) {
            Ok(f) => f,
            Err(e) => {
                eprintln!("[Spotter] Features error: {}", e);
                continue;
            }
        };

        // Check detectors
        for det in &mut detectors {
            let (triggered, prob) = det.process(&features);
            if prob > 0.08 {
                eprintln!("[Spotter Probe] {} prob: {:.3} (thresh: {:.2})", det.name, prob, det.threshold);
            }
            if triggered {
                eprintln!("[Spotter] >>> DETECTED: {} (conf: {:.3}) <<<", det.name, prob);
                let msg = format!("WAKE {} {:.3}\n", det.name, prob);
                let _ = stdout.write_all(msg.as_bytes());
                let _ = stdout.flush();
            }
        }
    }

    Ok(())
}
