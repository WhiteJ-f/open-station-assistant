#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <dlfcn.h>
#include <unistd.h>
#include "/tmp/rhvoice_dev/RHVoice.h"

typedef struct {
    FILE *out;
    int sample_rate;
    unsigned int total_samples;
    double volume;
    short prev_sample;
    int has_prev;
} SynthContext;

static int cb_set_sample_rate(int sample_rate, void *user_data) {
    SynthContext *ctx = (SynthContext*)user_data;
    ctx->sample_rate = sample_rate;
    // 44-byte WAV header reserved for 48000 Hz stereo
    fseek(ctx->out, 44, SEEK_SET);
    return 1;
}

// Convert 24kHz mono to 48kHz stereo with volume scaling and linear interpolation
static int cb_play_speech(const short *samples, unsigned int count, void *user_data) {
    SynthContext *ctx = (SynthContext*)user_data;
    if (count == 0) return 1;

    // Buffer for 48kHz stereo (each 24kHz mono sample produces 2 stereo frames = 4 shorts)
    short out_buf[1024 * 4];
    unsigned int chunk_size = 1024;

    for (unsigned int i = 0; i < count; i += chunk_size) {
        unsigned int cur = count - i;
        if (cur > chunk_size) cur = chunk_size;

        unsigned int out_idx = 0;
        for (unsigned int j = 0; j < cur; j++) {
            short s_curr = samples[i + j];
            short s_prev = ctx->has_prev ? ctx->prev_sample : s_curr;
            short s_mid = (short)(((int)s_prev + (int)s_curr) / 2);
            ctx->prev_sample = s_curr;
            ctx->has_prev = 1;

            // Interpolated middle frame (left & right)
            short v_mid = (short)(s_mid * ctx->volume);
            out_buf[out_idx++] = v_mid;
            out_buf[out_idx++] = v_mid;

            // Current frame (left & right)
            short v_curr = (short)(s_curr * ctx->volume);
            out_buf[out_idx++] = v_curr;
            out_buf[out_idx++] = v_curr;
        }

        size_t written = fwrite(out_buf, sizeof(short), out_idx, ctx->out);
        ctx->total_samples += written / 2; // in stereo frames
        if (written != out_idx) return 0;
    }
    return 1;
}

static void write_wav_header_48k_stereo(FILE *f, unsigned int total_frames) {
    unsigned int sample_rate = 48000;
    short num_channels = 2; // Stereo
    short bits_per_sample = 16;
    short block_align = num_channels * (bits_per_sample / 8); // 4 bytes
    unsigned int byte_rate = sample_rate * block_align; // 192000
    unsigned int data_size = total_frames * block_align;
    unsigned int file_size = 36 + data_size;
    short audio_format = 1; // PCM

    fseek(f, 0, SEEK_SET);
    fwrite("RIFF", 1, 4, f);
    fwrite(&file_size, 4, 1, f);
    fwrite("WAVEfmt ", 1, 8, f);
    unsigned int fmt_size = 16;
    fwrite(&fmt_size, 4, 1, f);
    fwrite(&audio_format, 2, 1, f);
    fwrite(&num_channels, 2, 1, f);
    fwrite(&sample_rate, 4, 1, f);
    fwrite(&byte_rate, 4, 1, f);
    fwrite(&block_align, 2, 1, f);
    fwrite(&bits_per_sample, 2, 1, f);
    fwrite("data", 1, 4, f);
    fwrite(&data_size, 4, 1, f);
}

int main(int argc, char **argv) {
    const char *lib_path = "/bkupgrade/rhvoice/lib/libRHVoice.so.5";
    const char *data_dir = "/bkupgrade/rhvoice/data";
    const char *voice_name = "Elena";
    const char *output_file = "/tmp/rhvoice_out.wav";
    const char *text = NULL;
    double rate = 1.0;
    double pitch = 1.0;
    double volume = 0.15; // default 15% volume

    for (int i = 1; i < argc; i++) {
        if (strcmp(argv[i], "-l") == 0 && i + 1 < argc) lib_path = argv[++i];
        else if (strcmp(argv[i], "-d") == 0 && i + 1 < argc) data_dir = argv[++i];
        else if (strcmp(argv[i], "-v") == 0 && i + 1 < argc) voice_name = argv[++i];
        else if (strcmp(argv[i], "-o") == 0 && i + 1 < argc) output_file = argv[++i];
        else if (strcmp(argv[i], "-r") == 0 && i + 1 < argc) rate = atof(argv[++i]);
        else if (strcmp(argv[i], "-p") == 0 && i + 1 < argc) pitch = atof(argv[++i]);
        else if (strcmp(argv[i], "-vol") == 0 && i + 1 < argc) volume = atof(argv[++i]);
        else if (strcmp(argv[i], "-t") == 0 && i + 1 < argc) text = argv[++i];
    }

    if (!text || strlen(text) == 0) {
        text = "Я вас слушаю";
    }

    void *h = dlopen(lib_path, RTLD_LAZY | RTLD_GLOBAL);
    if (!h) {
        fprintf(stderr, "dlopen failed: %s\n", dlerror());
        return 1;
    }

    RHVoice_tts_engine (*fn_new_tts_engine)(const RHVoice_init_params*);
    void (*fn_delete_tts_engine)(RHVoice_tts_engine);
    RHVoice_message (*fn_new_message)(RHVoice_tts_engine, const char*, unsigned int, RHVoice_message_type, const RHVoice_synth_params*, void*);
    void (*fn_delete_message)(RHVoice_message);
    int (*fn_speak)(RHVoice_message);

    fn_new_tts_engine = dlsym(h, "RHVoice_new_tts_engine");
    fn_delete_tts_engine = dlsym(h, "RHVoice_delete_tts_engine");
    fn_new_message = dlsym(h, "RHVoice_new_message");
    fn_delete_message = dlsym(h, "RHVoice_delete_message");
    fn_speak = dlsym(h, "RHVoice_speak");

    if (!fn_new_tts_engine || !fn_delete_tts_engine || !fn_new_message || !fn_delete_message || !fn_speak) {
        fprintf(stderr, "dlsym symbols failed\n");
        dlclose(h);
        return 1;
    }

    FILE *out = fopen(output_file, "wb");
    if (!out) {
        perror("fopen output");
        dlclose(h);
        return 1;
    }

    SynthContext ctx;
    memset(&ctx, 0, sizeof(ctx));
    ctx.out = out;
    ctx.volume = volume;

    RHVoice_init_params init_params;
    memset(&init_params, 0, sizeof(init_params));
    init_params.data_path = data_dir;
    init_params.callbacks.set_sample_rate = cb_set_sample_rate;
    init_params.callbacks.play_speech = cb_play_speech;

    RHVoice_tts_engine engine = fn_new_tts_engine(&init_params);
    if (!engine) {
        fprintf(stderr, "RHVoice_new_tts_engine failed\n");
        fclose(out);
        dlclose(h);
        return 1;
    }

    RHVoice_synth_params synth_params;
    memset(&synth_params, 0, sizeof(synth_params));
    synth_params.voice_profile = voice_name;
    synth_params.relative_rate = rate;
    synth_params.relative_pitch = pitch;
    synth_params.relative_volume = 1.0;

    RHVoice_message msg = fn_new_message(engine, text, strlen(text), RHVoice_message_text, &synth_params, &ctx);
    if (!msg) {
        fprintf(stderr, "RHVoice_new_message failed\n");
        fn_delete_tts_engine(engine);
        fclose(out);
        dlclose(h);
        return 1;
    }

    fn_speak(msg);

    write_wav_header_48k_stereo(out, ctx.total_samples);
    fclose(out);

    fn_delete_message(msg);
    fn_delete_tts_engine(engine);
    dlclose(h);
    return 0;
}
