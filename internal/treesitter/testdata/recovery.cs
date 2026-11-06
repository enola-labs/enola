            if (value.TryFormat(_writeBuffer.AvailableSpan, out int bytesWritten, "X"))
            {
                _writeBuffer.Commit(bytesWritten);
            if (async)
            {
        private ValueTask FlushAsync(bool async)
        {
            ReadOnlyMemory<byte> bytes = _writeBuffer.ActiveMemory;
            if (bytes.Length > 0)
                _writeBuffer.Discard(bytes.Length);
            if (NetEventSource.Log.IsEnabled()) Trace($"Writing {source.Length} bytes.");