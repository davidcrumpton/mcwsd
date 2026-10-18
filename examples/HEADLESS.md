# Headless Server Usage

MCWSD exposes an API on its port and there is no requirement for an AI or  graphical interface.  However, I haven't written any code for a headless client yet except the test [Python script](../tools/openwebui/mcwsd/test_mcwsd_cli.py) can be used as a reference.

If you are scripting provisioning containers, you probably would have already written a script to call provisioning commands like `docker run` or `kubectl apply`.
