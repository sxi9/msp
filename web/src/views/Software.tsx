import { useRef, useState } from 'preact/hooks';
import { api, errorText } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { when } from '../lib/format.ts';
import { fileSize, shortHash } from '../lib/deploy.ts';
import type { LibraryFile, Org, User } from '../types.ts';
import { ConfirmButton } from '../components/Confirm.tsx';
import { CopyButton } from '../components/CopyBlock.tsx';

/** The name of a client org by id, from the loaded list; a fallback when unknown. */
function orgName(orgs: Org[], id: number): string {
  return orgs.find((o) => o.id === id)?.name ?? `#${id}`;
}

/**
 * The Software library page (SD1, admin/operator): upload an installer or file
 * once, then deploy it to a device from its page. The table is org-scoped by the
 * server; a global file ("All clients") is usable by any client. The bytes never
 * reach the browser — only the metadata and the sha256 do.
 */
export function Software({ user }: { user: User }) {
  const files = usePoll(() => api.listFiles().then((r) => r.files), [], 15000);
  const orgs = usePoll(() => api.listOrgs().then((r) => r.orgs), [], 0);
  const [error, setError] = useState('');

  const remove = async (f: LibraryFile) => {
    setError('');
    try {
      await api.deleteFile(f.id);
      files.reload();
    } catch (e) {
      setError(errorText(e));
    }
  };

  const orgList = orgs.data ?? [];
  return (
    <section>
      <div class="toolbar"><h1>Software library</h1></div>
      <p class="muted">
        Upload an installer or file once, then deploy it to a device from its page. Agents download it over
        their authenticated channel and verify the SHA-256 before use; every upload, delete and deploy is audited.
      </p>
      <UploadForm user={user} orgs={orgList} onUploaded={() => files.reload()} />
      {error && <p class="error">{error}</p>}
      {files.error && <p class="error">{files.error}</p>}

      <h2>Files</h2>
      {!files.data ? <p class="muted">Loading…</p> : files.data.length === 0 ? (
        <p class="empty">No files yet. Upload one above.</p>
      ) : (
        <div class="log-scroll">
          <table class="grid">
            <thead>
              <tr><th>Name</th><th>Size</th><th>SHA-256</th><th>Client</th><th>Uploaded by</th><th>Date</th><th></th></tr>
            </thead>
            <tbody>
              {files.data.map((f) => (
                <tr key={f.id}>
                  <td data-label="Name"><code>{f.name}</code></td>
                  <td data-label="Size">{fileSize(f.size)}</td>
                  <td data-label="SHA-256" class="muted"><code title={f.sha256}>{shortHash(f.sha256)}…</code> <CopyButton text={f.sha256} /></td>
                  <td data-label="Client">{f.org_id === null ? <span class="chip">All clients</span> : orgName(orgList, f.org_id)}</td>
                  <td data-label="Uploaded by" class="muted">{f.created_by_email || '—'}</td>
                  <td data-label="Date" class="muted">{when(f.created_at)}</td>
                  <td data-label=""><ConfirmButton label="Delete" question={`Delete ${f.name}? Devices can no longer download it.`} danger onConfirm={() => remove(f)} /></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

function UploadForm({ user, orgs, onUploaded }: { user: User; orgs: Org[]; onUploaded: () => void }) {
  const [name, setName] = useState('');
  const [orgId, setOrgId] = useState(''); // '' = global (all clients)
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [ok, setOk] = useState('');
  const fileRef = useRef<HTMLInputElement>(null);

  const submit = async (e: Event) => {
    e.preventDefault();
    const input = fileRef.current;
    const file = input?.files?.[0];
    if (!file) {
      setError('Choose a file to upload.');
      return;
    }
    setBusy(true);
    setError('');
    setOk('');
    try {
      const res = await api.uploadFile(file, name.trim() || file.name, orgId === '' ? null : Number(orgId));
      setOk(`Uploaded ${res.file.name} (${fileSize(res.file.size)}).`);
      setName('');
      if (input) input.value = '';
      onUploaded();
    } catch (e) {
      const t = errorText(e);
      setError(t.startsWith('file_too_large') ? 'The file is larger than the server limit (MSP_MAX_FILE_MB).'
        : t.startsWith('forbidden') ? 'A global file needs access to all clients; pick a client instead.' : t);
    } finally {
      setBusy(false);
    }
  };

  return (
    <form class="card" onSubmit={submit}>
      <h2>Upload a file</h2>
      <div class="form-row">
        <input type="file" ref={fileRef} onInput={() => { setError(''); setOk(''); }} />
        <input type="text" placeholder="Name (defaults to the file name)" value={name} maxLength={255} onInput={(e) => setName(e.currentTarget.value)} />
        <label>Client
          <select value={orgId} onChange={(e) => setOrgId(e.currentTarget.value)}>
            <option value="">All clients (global)</option>
            {orgs.map((o) => <option key={o.id} value={String(o.id)}>{o.name}</option>)}
          </select>
        </label>
        <button type="submit" class="btn btn-primary" disabled={busy}>{busy ? 'Uploading…' : 'Upload'}</button>
      </div>
      {error && <p class="error">{error}</p>}
      {ok && <p class="ok">{ok}</p>}
      <p class="muted">
        A global file is usable for any client; a client-scoped file only for that client's devices
        {user.role === 'operator' ? ' you can see' : ''}.
      </p>
    </form>
  );
}
