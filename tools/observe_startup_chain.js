'use strict';
// Observation only: no replacements, argument/return changes or memory writes.
// Loaded only by observe_startup_chain.py into the host it creates and its BROWSERs.
const observedModules = new Set();
function emit(event, data) {
    send(Object.assign({event: event, pid: Process.id,
        tid: Process.getCurrentThreadId(), time_ms: Date.now()}, data || {}));
}
function attempt(fn) { try { return fn(); } catch (_) { return null; } }
function trace(ctx) {
    return attempt(() => Thread.backtrace(ctx, Backtracer.ACCURATE).slice(0, 10)
        .map(a => { const m = Process.findModuleByAddress(a);
            return m ? m.name + '+' + a.sub(m.base) : a.toString(); }));
}
function runtimeInfo(p) {
    return {runtime: p.toString(),
        version: attempt(() => p.add(0x3c0).readU32()),
        preload: attempt(() => p.add(0x550).readU8()),
        appid: attempt(() => {
            const s = p.add(0x98);
            const mark = s.add(23).readS8();
            const len = mark < 0 ? s.add(8).readU64().toNumber() : mark;
            if (len < 0 || len > 80) return 'invalid';
            const value = (mark < 0 ? s.readPointer() : s).readUtf8String(len);
            return /^(wx[0-9a-f]{16}|preload-[A-Za-z0-9_-]*)$/.test(value) ? value : 'other';
        })};
}
function hook(m, rva, event, enter, leave, firstIndex) {
    try {
        Interceptor.attach(m.base.add(rva), {
            onEnter(args) {
                this.first = args[firstIndex || 0];
                const data = enter ? attempt(() => enter(args, this.context)) : {};
                emit(event, data);
            },
            onLeave(ret) {
                if (leave) emit(event + '.return', attempt(() => leave(this.first, ret)));
            }
        });
    } catch (error) { emit('hook_error', {event_name:event, error:String(error)}); }
}
function installHost(m) {
    const proxy = a => ({proxy:a[0].toString()});
    hook(m, 0x1bafc0, 'host.InitCore', proxy);
    hook(m, 0x1ba090, 'host.ReBind', proxy);
    hook(m, 0x1bbc50, 'host.LaunchAndBind', proxy);
    hook(m, 0x1bd350, 'host.OnChannelConnected', a => ({proxy:a[0].sub(8).toString()}));
    hook(m, 0x0d79b0, 'host.OnChannelConnected.response');
    hook(m, 0x1be860, 'host.Heartbeat', a => ({proxy:a[0].sub(8).toString()}));
    hook(m, 0x1bd950, 'host.CheckHeartbeat', a => ({proxy:a[0].toString(),
        received: a[0].add(401).readU8(), suspended:a[0].add(402).readU8()}));
    hook(m, 0x1bab80, 'host.TerminateBrowser', (a,c) => ({proxy:a[0].toString(), stack:trace(c)}));
    hook(m, 0x1bc060, 'host.Reset', (a,c) => ({proxy:a[0].toString(), stack:trace(c)}));
    hook(m, 0x1be940, 'host.ReLaunch', proxy);
    hook(m, 0x1bd1c0, 'host.EnsureConnected.timeout', proxy);
}
function installBrowser(m) {
    const ilinkInfo = p => ({instance:p.toString(),
        initialized:attempt(() => p.add(305).readU8()),
        logged_in:attempt(() => p.add(306).readU8())});
    hook(m, 0x2256160, 'ilink.Initialize', a => Object.assign(ilinkInfo(a[0]),
        {sdk_mode:a[4].toUInt32() & 255}), ilinkInfo);
    // ThirdAppLogin's secondary this points 16 bytes into the same instance.
    hook(m, 0x2260fd0, 'ilink.ThirdAppLogin', a => ilinkInfo(a[0].sub(16)),
        p => ilinkInfo(p.sub(16)));
    hook(m, 0x2261a00, 'ilink.ThirdAppLogin.result', a => Object.assign(ilinkInfo(a[0]),
        {login_type:a[4].toUInt32(), error_code:a[5].toInt32()}), ilinkInfo);
    hook(m, 0x2260ee0, 'ilink.Login.result', a => Object.assign(ilinkInfo(a[0]),
        {login_type:a[2].toUInt32(), error_code:a[3].toInt32()}), ilinkInfo);
    hook(m, 0x22622e0, 'ilink.Logout.result', a => Object.assign(ilinkInfo(a[0]),
        {error_code:a[2].toUInt32()}), ilinkInfo);
    hook(m, 0x225da70, 'ilink.LaunchApplet', a => ilinkInfo(a[0]));
    hook(m, 0x2376870, 'browser.Initialize', a => ({runtime_impl:a[0].toString()}),
        p => ({runtime_impl:p.toString(), system_version:p.add(32).readU32()}));
    hook(m, 0x26bf0b0, 'browser.OnChannelConnected.send');
    hook(m, 0x2378650, 'browser.OnChannelConnected.ack', a => ({runtime_impl:a[0].toString(),
        timer:a[0].add(0x1b8).readPointer().toString()}));
    hook(m, 0x2377f20, 'browser.QuitMainLoop', (a,c) => ({runtime_impl:a[0].toString(),
        reason:a[1].toUInt32(), stack:trace(c)}));
    hook(m, 0x2379250, 'browser.DoHeartbeat');
    hook(m, 0x23794b0, 'browser.Heartbeat.ack');
    hook(m, 0x2379a90, 'browser.CheckHeartbeat', a => ({runtime_impl:a[0].toString(),
        received:a[0].add(352).readU8(), suspended:a[0].add(152).readU8()}));
    hook(m, 0x237a080, 'browser.Heartbeat.fallback');
    hook(m, 0x18d8850, 'browser.TerminateSelf', (a,c) => ({code:a[0].toUInt32(), stack:trace(c)}));
    hook(m, 0x26ffd00, 'config.Parse', a => ({config:a[1].toString()}),
        p => ({config:p.toString(), version:p.add(0x308).readU32()}), 1);
    hook(m, 0x259f6c0, 'runtime.Construct', a => ({runtime:a[0].toString(),
        config:a[3].toString(), source_version:a[3].add(0x308).readU32()}), runtimeInfo);
    hook(m, 0x25a4ac0, 'config.Assign', a => ({config:a[0].toString(),
        source_version:a[1].add(0x308).readU32()}),
        p => ({config:p.toString(), version:p.add(0x308).readU32()}));
    hook(m, 0x25ac020, 'runtime.StartPreload', a => runtimeInfo(a[0]));
    hook(m, 0x25ac620, 'runtime.ReusePreload', a => Object.assign(runtimeInfo(a[0]),
        {source_version:a[2].add(0x308).readU32()}), runtimeInfo);
    hook(m, 0x25de1d0, 'container.OnLoadStart', a => Object.assign(
        runtimeInfo(a[0].add(64).readPointer()),
        {container:a[0].toString(), is_main_frame:a[1].toUInt32() & 255}));
    hook(m, 0x2bac420, 'devtools.Start', a => Object.assign(runtimeInfo(a[0].add(8).readPointer()),
        {server:a[0].toString(), mode:a[0].add(41).readU8()}));
    hook(m, 0x7da8170, 'devtools.SetupContext', a => Object.assign(
        runtimeInfo(a[0].add(8).readPointer()), {server:a[0].toString()}));
}
function installApi(m) {
    if (!/^(kernel32|kernelbase)\.dll$/i.test(m.name)) return;
    const terminate = m.findExportByName('TerminateProcess');
    if (terminate && !observedModules.has(terminate.toString())) {
        observedModules.add(terminate.toString());
        Interceptor.attach(terminate, {onEnter(a) {
            emit('api.TerminateProcess', {handle:a[0].toString(), code:a[1].toUInt32(), stack:trace(this.context)});
        }});
    }
    const exit = m.findExportByName('ExitProcess');
    if (exit && !observedModules.has(exit.toString())) {
        observedModules.add(exit.toString());
        Interceptor.attach(exit, {onEnter(a) {
            emit('api.ExitProcess', {code:a[0].toUInt32(), stack:trace(this.context)});
        }});
    }
}
Process.attachModuleObserver({onAdded(m) {
    if (m.name.toLowerCase() === 'wmpf_host_export_x64.dll') {
        installHost(m); emit('installed.host', {base:m.base.toString()});
    } else if (m.name.toLowerCase() === 'flue.dll') {
        installBrowser(m); emit('installed.browser', {base:m.base.toString()});
    }
    installApi(m);
}});
emit('observer_ready');
