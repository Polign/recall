"""Durability smoke test. Install the Python package, then set RECALL_TEST_POLIGN
and RECALL_TEST_SERVER to built binaries and run this script. Uses temporary
storage and local ports; never connects to the caller's database.
"""
import os, subprocess, sys, tempfile, time, urllib.request, socket
from polign_recall import Client

with tempfile.TemporaryDirectory(prefix='recall-release-') as directory:
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0)); port = sock.getsockname()[1]
    url = f'http://127.0.0.1:{port}'
    env = {**os.environ, 'POLIGN_URL':url, 'RECALL_TEST_POLIGN':os.environ['RECALL_TEST_POLIGN'], 'PYTHONDONTWRITEBYTECODE':'1'}
    log = open(directory+'/server.log','w+')
    def start():
        p = subprocess.Popen([os.environ['RECALL_TEST_SERVER'],'-store','fs:'+directory+'/data','-http',f'127.0.0.1:{port}','-grpc','127.0.0.1:0','-telemetry=false'],stdout=log,stderr=log,env=env)
        for _ in range(100):
            if p.poll() is not None:
                log.seek(0); raise RuntimeError(log.read())
            try:
                with urllib.request.urlopen(url+'/v1/collections/recall_lexical_v1/watermark',timeout=1) as response:
                    if response.status==200: return p
            except Exception: time.sleep(.1)
        p.terminate(); p.wait(); raise RuntimeError('server startup timed out')
    def stop(p):
        p.terminate()
        try: p.wait(timeout=15)
        except subprocess.TimeoutExpired: p.kill();p.wait();raise
    def client():
        return Client(command=[os.environ['RECALL_TEST_POLIGN'],'mcp','-memory-only','-write'],env=env)
    server=start()
    try:
        subprocess.run([sys.executable,'-m','unittest','discover','-s',str(__import__('pathlib').Path(__file__).parent),'-v'],env=env,check=True)
        with client() as c:
            first=c.remember('restart-check','prefers_editor','vim')
            c.remember('restart-check','prefers_editor','neovim')
    finally: stop(server)
    server=start()
    try:
        with client() as c:
            assert c.recall('restart-check','prefers_editor')[0].value=='neovim'
            assert c.recall('restart-check','prefers_editor',as_of=first.stored.observed_at)[0].value=='vim'
            assert c.forget('restart-check','prefers_editor','neovim')==1
    finally: stop(server)
    server=start()
    try:
        with client() as c:
            assert c.recall('restart-check','prefers_editor')==[]
            assert len(c.history('restart-check','prefers_editor'))==3
    finally: stop(server)
    print('PASS: Python MCP integration, server restart, historical recall, and durable retraction')
