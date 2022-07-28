use std::io::{stdin, stdout, Read, Write, ErrorKind};
use wasmer::{Instance, Module, Store};
use wasmer_engine_universal::Universal;
use wasmer_wasi::{Stdin, Stdout, Stderr, WasiState};
use wasmer_types::{Value};

fn main() -> Result<(), Box<dyn std::error::Error>> {
    // Read 4 bytes from stdin, interpret as little-endian uint32.
    let mut buf = [0u8; 4];
    stdin().read_exact(&mut buf)?;
    let wasm_size = u32::from_le_bytes(buf);

    // Read `wasm_size` bytes and put it into `wasm_bytes`.
    let mut wasm_bytes = vec![0u8; wasm_size as usize];
    stdin().read_exact(&mut wasm_bytes)?;

    #[cfg(feature = "llvm")]
    let compiler = wasmer_compiler_llvm::LLVM::default();
    #[cfg(not(feature = "llvm"))]
    let compiler = wasmer_compiler_cranelift::Cranelift::default();

    let store = Store::new(&Universal::new(compiler).engine());

    // Let's compile the Wasm module.
    let module = Module::new(&store, wasm_bytes)?;

    let mut wasi_env = WasiState::new("peasant")
        .stdin(Box::new(Stdin))
        .stdout(Box::new(Stdout))
        .stderr(Box::new(Stderr))
        .finalize()?;

    // Then, we get the import object related to our WASI
    // and attach it to the Wasm instance.
    let import_object = wasi_env.import_object(&module)?;
    let instance = Instance::new(&module, &import_object)?;

    // Initialize the code.
    let start = instance.exports.get_function("_start")?;
    start.call(&[])?;

    let process_stdio = instance.exports.get_function("ProcessStdio")?;

    // Restrict ourselves
    let _ = rlimit::setrlimit(rlimit::Resource::CORE, 0, 0);
    let _ = rlimit::setrlimit(rlimit::Resource::FSIZE, 0, 0);
    let _ = rlimit::setrlimit(rlimit::Resource::KQUEUES, 0, 0);
    let _ = rlimit::setrlimit(rlimit::Resource::LOCKS, 0, 0);
    let _ = rlimit::setrlimit(rlimit::Resource::MEMLOCK, 0, 0);
    let _ = rlimit::setrlimit(rlimit::Resource::MSGQUEUE, 0, 0);
    let _ = rlimit::setrlimit(rlimit::Resource::NOFILE, 0, 0);
    let _ = rlimit::setrlimit(rlimit::Resource::NOVMON, 0, 0);
    let _ = rlimit::setrlimit(rlimit::Resource::NPROC, 0, 0);
    let _ = rlimit::setrlimit(rlimit::Resource::NPTS, 0, 0);
    let _ = rlimit::setrlimit(rlimit::Resource::NTHR, 0, 0);
    let _ = rlimit::setrlimit(rlimit::Resource::POSIXLOCKS, 0, 0);
    let _ = rlimit::setrlimit(rlimit::Resource::UMTXP, 0, 0);
    if nix::unistd::geteuid().is_root() {
        privdrop::PrivDrop::default()
            .chroot("/var/empty")
            .user("nobody")
            .apply()?;
    }
    #[cfg(target_os = "linux")]
    let _ = scheduler::set_self_policy(scheduler::Policy::Idle, 0);

    // Write 0,0,0,0 to stdout
    {
        let word: u32 = 0x00000000;
        let bytes = word.to_le_bytes();
        let mut so = stdout();
        so.write_all(&bytes).unwrap();
        so.flush()?;
    }

    // In a loop, read another uint32 and call ProcessStdio(N)
    loop {
        let mut buf = [0u8; 4];
        match stdin().read_exact(&mut buf) {
            Ok(_v) => {},
            Err(ref e) if e.kind() == ErrorKind::UnexpectedEof => break,
            Err(e) => return Err(Box::new(e)),
        }
        let size = u32::from_le_bytes(buf);

        process_stdio.call(&[Value::I32(size as i32)])?;
    }

    Ok(())
}
