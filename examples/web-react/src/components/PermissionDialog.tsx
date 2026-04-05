import { useState } from 'react';
import type { PermissionRequest } from '../types';

interface PermissionDialogProps {
  permission: PermissionRequest;
  onApprove: (alwaysAllow?: boolean) => void;
  onDeny: (reason?: string) => void;
}

export function PermissionDialog({ permission, onApprove, onDeny }: PermissionDialogProps) {
  const [alwaysAllow, setAlwaysAllow] = useState(false);
  const [denyReason, setDenyReason] = useState('');

  const formatInput = (input: unknown): string => {
    if (typeof input === 'string') return input;
    try {
      return JSON.stringify(input, null, 2);
    } catch {
      return String(input);
    }
  };

  return (
    <div className="dialog-overlay">
      <div className="dialog permission-dialog">
        <h2>Permission Request</h2>

        <div className="dialog-content">
          <div className="permission-info">
            <div className="info-row">
              <span className="label">Tool:</span>
              <span className="value tool-name">{permission.tool_name}</span>
            </div>
            <div className="info-row">
              <span className="label">Level:</span>
              <span className="value">{permission.permission}</span>
            </div>
          </div>

          <div className="tool-input-section">
            <label>Tool Input:</label>
            <pre className="tool-input">{formatInput(permission.tool_input)}</pre>
          </div>

          <div className="dialog-actions">
            <div className="primary-actions">
              <button
                className="btn btn-approve"
                onClick={() => onApprove(alwaysAllow)}
              >
                Approve
              </button>
              <button
                className="btn btn-deny"
                onClick={() => onDeny(denyReason)}
              >
                Deny
              </button>
            </div>

            <div className="secondary-actions">
              <label className="checkbox-label">
                <input
                  type="checkbox"
                  checked={alwaysAllow}
                  onChange={(e) => setAlwaysAllow(e.target.checked)}
                />
                Always allow for this tool
              </label>
            </div>
          </div>

          {denyReason === '' && (
            <div className="deny-reason-section">
              <label>Reason (optional):</label>
              <input
                type="text"
                value={denyReason}
                onChange={(e) => setDenyReason(e.target.value)}
                placeholder="Why are you denying this?"
                className="deny-reason-input"
              />
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
